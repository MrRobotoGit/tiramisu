package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrAlreadyRunning is returned when TriggerRun is called on a job that is already running.
var ErrAlreadyRunning = errors.New("job already running")

// JobState tracks the runtime state of a scheduled job.
type JobState struct {
	LastRun    time.Time `json:"last_run,omitempty"`
	NextRun    time.Time `json:"next_run,omitempty"`
	Running    bool      `json:"running"`
	LastError  string    `json:"last_error,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
}

// Syncer is the interface all sync engines must implement.
type Syncer interface {
	Name() string
	Run(ctx context.Context) error
}

// ErrNotRunning is returned when StopJob is called on a job that is not running.
var ErrNotRunning = errors.New("job not running")

// Scheduler manages scheduled and manual sync jobs.
type Scheduler struct {
	cfg     SchedulerConfig
	jobs    map[string]Syncer
	state   *StateStore
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// SchedulerConfig mirrors the config.go struct to avoid import cycles.
type SchedulerConfig struct {
	Enabled       bool
	MoviesSync    DailyJobConfig
	TVSync        DailyJobConfig
	WatchlistSync WatchlistSyncConfig
}

// DailyJobConfig mirrors config.go.
type DailyJobConfig struct {
	Enabled    bool
	DaysOfWeek []int
	Hour       int
	Minute     int
}

// WatchlistSyncConfig mirrors config.go.
type WatchlistSyncConfig struct {
	Enabled       bool
	IntervalHours int
}

// New creates a Scheduler.
func New(cfg SchedulerConfig, jobs map[string]Syncer, statePath string) *Scheduler {
	ss, _ := NewStateStore(statePath)

	s := &Scheduler{
		cfg:     cfg,
		jobs:    jobs,
		state:   ss,
		cancels: make(map[string]context.CancelFunc),
	}

	// Pre-populate trackers for all jobs and reset stale running state
	for name := range jobs {
		jt := ss.Tracker(name)
		jt.SetRunning(false) // Reset stale running state from previous run
	}

	// Calculate initial NextRun times
	s.updateNextRuns()

	return s
}

// Tracker returns the JobTracker for a named job, creating if needed.
func (s *Scheduler) Tracker(name string) *JobTracker {
	return s.state.Tracker(name)
}

// Run starts the scheduler loop. Blocks until stop is closed.
func (s *Scheduler) Run(stop <-chan struct{}) {
	logger := log.New(os.Stdout, "[Scheduler] ", log.LstdFlags)
	logger.Printf("started (tick=60s)")

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			logger.Printf("stopping")
			s.state.Save()
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// TriggerRun starts a job immediately. Returns ErrAlreadyRunning if the job is already running.
func (s *Scheduler) TriggerRun(name string) error {
	syncer, ok := s.jobs[name]
	if !ok {
		return fmt.Errorf("unknown job: %s", name)
	}

	jt := s.state.Tracker(name)
	if jt.Snapshot().Running {
		return ErrAlreadyRunning
	}

	// V2.0: Set running before spawn to prevent concurrent launches from tick().
	jt.SetRunning(true)
	s.state.Save()
	go s.runJob(syncer, jt)
	return nil
}

// StopJob cancels a running job. Returns ErrNotRunning if the job is not running.
func (s *Scheduler) StopJob(name string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[name]
	s.mu.Unlock()

	if !ok {
		return ErrNotRunning
	}

	cancel()
	log.Printf("[Scheduler] %s stop requested", name)
	return nil
}

// Status returns a snapshot of all job states.
func (s *Scheduler) Status() map[string]JobState {
	return s.state.Status()
}

func (s *Scheduler) tick() {
	for name, syncer := range s.jobs {
		jt := s.state.Tracker(name)
		state := jt.Snapshot()

		if !s.shouldRun(name, state) {
			continue
		}

		// V2.0: Set running before spawn to prevent concurrent TriggerRun/tick races.
		jt.SetRunning(true)
		go s.runJob(syncer, jt)
	}

	s.updateNextRuns()
	s.state.Save()
}

func (s *Scheduler) shouldRun(name string, state JobState) bool {
	if state.Running {
		return false
	}

	switch name {
	case "movies":
		return s.shouldRunDaily(state, s.cfg.MoviesSync.Enabled, s.cfg.MoviesSync.DaysOfWeek, s.cfg.MoviesSync.Hour, s.cfg.MoviesSync.Minute)
	case "tv":
		return s.shouldRunDaily(state, s.cfg.TVSync.Enabled, s.cfg.TVSync.DaysOfWeek, s.cfg.TVSync.Hour, s.cfg.TVSync.Minute)
	case "watchlist":
		return s.shouldRunInterval(state, s.cfg.WatchlistSync.Enabled, s.cfg.WatchlistSync.IntervalHours)
	}

	return false
}

func (s *Scheduler) shouldRunDaily(state JobState, enabled bool, daysOfWeek []int, hour, minute int) bool {
	if !enabled {
		return false
	}

	now := time.Now()
	if !state.NextRun.IsZero() && now.Before(state.NextRun) {
		return false
	}

	daySet := make(map[int]bool)
	for _, d := range daysOfWeek {
		daySet[d] = true
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !daySet[int(today.Weekday())] {
		return false
	}

	if now.Before(today) {
		return false
	}

	if !state.LastRun.IsZero() {
		lastDay := time.Date(state.LastRun.Year(), state.LastRun.Month(), state.LastRun.Day(), 0, 0, 0, 0, now.Location())
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if !lastDay.Before(todayStart) {
			return false
		}
	}

	return true
}

func (s *Scheduler) shouldRunInterval(state JobState, enabled bool, intervalHours int) bool {
	if !enabled || intervalHours <= 0 {
		return false
	}

	if state.LastRun.IsZero() {
		return true
	}

	return time.Since(state.LastRun) >= time.Duration(intervalHours)*time.Hour
}

func (s *Scheduler) runJob(syncer Syncer, jt *JobTracker) {
	name := syncer.Name()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Scheduler] %s PANIC: %v", name, r)
			jt.SetRunning(false)
			jt.SetStatus("failed", fmt.Sprintf("panic: %v", r))
			s.state.Save()
		}
		s.mu.Lock()
		delete(s.cancels, name)
		s.mu.Unlock()
	}()

	s.state.Save()
	log.Printf("[Scheduler] %s started", name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.mu.Lock()
	s.cancels[name] = cancel
	s.mu.Unlock()

	err := syncer.Run(ctx)

	jt.SetRunning(false)
	if err != nil && ctx.Err() == nil {
		log.Printf("[Scheduler] %s failed: %v", name, err)
		jt.SetStatus("failed", err.Error())
	} else if ctx.Err() != nil {
		log.Printf("[Scheduler] %s stopped by user", name)
		jt.SetStatus("stopped", "")
	} else {
		log.Printf("[Scheduler] %s completed", name)
		jt.SetStatus("ok", "")
	}

	s.state.Save()
}

func (s *Scheduler) updateNextRuns() {
	for name := range s.jobs {
		jt := s.state.Tracker(name)
		state := jt.Snapshot()

		var next time.Time
		switch name {
		case "movies":
			next = nextRunTime(s.cfg.MoviesSync.Enabled, s.cfg.MoviesSync.DaysOfWeek, s.cfg.MoviesSync.Hour, s.cfg.MoviesSync.Minute)
		case "tv":
			next = nextRunTime(s.cfg.TVSync.Enabled, s.cfg.TVSync.DaysOfWeek, s.cfg.TVSync.Hour, s.cfg.TVSync.Minute)
		case "watchlist":
			if s.cfg.WatchlistSync.Enabled && s.cfg.WatchlistSync.IntervalHours > 0 {
				next = state.LastRun.Add(time.Duration(s.cfg.WatchlistSync.IntervalHours) * time.Hour)
			}
		}

		if !next.Equal(state.NextRun) {
			jt.SetNextRun(next)
		}
	}
}

func nextRunTime(enabled bool, daysOfWeek []int, hour, minute int) time.Time {
	if !enabled {
		return time.Time{}
	}

	now := time.Now()
	daySet := make(map[int]bool)
	for _, d := range daysOfWeek {
		daySet[d] = true
	}

	for offset := 0; offset < 8; offset++ {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()).AddDate(0, 0, offset)
		if offset == 0 {
			if candidate.After(now) && daySet[int(candidate.Weekday())] {
				return candidate
			}
			continue
		}
		if daySet[int(candidate.Weekday())] {
			return candidate
		}
	}

	return time.Time{}
}

// --- StateStore (embedded to avoid import cycles with engines) ---

// stateFile is the on-disk format.
type stateFile struct {
	Jobs map[string]*JobState `json:"jobs"`
}

// StateStore persists JobState to a JSON file with atomic writes.
type StateStore struct {
	path string
	mu   sync.Mutex
	jobs map[string]*JobTracker
}

// NewStateStore loads or creates the state file.
func NewStateStore(path string) (*StateStore, error) {
	ss := &StateStore{
		path: path,
		jobs: make(map[string]*JobTracker),
	}

	if data, err := os.ReadFile(path); err == nil {
		var sf stateFile
		if err := json.Unmarshal(data, &sf); err == nil {
			for name, js := range sf.Jobs {
				ss.jobs[name] = &JobTracker{state: *js}
			}
		}
	}

	return ss, nil
}

// Tracker returns the JobTracker for a named job, creating if needed.
func (ss *StateStore) Tracker(name string) *JobTracker {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if jt, ok := ss.jobs[name]; ok {
		return jt
	}
	jt := &JobTracker{}
	ss.jobs[name] = jt
	return jt
}

// Status returns a snapshot of all job states.
func (ss *StateStore) Status() map[string]JobState {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	result := make(map[string]JobState)
	for name, jt := range ss.jobs {
		result[name] = jt.Snapshot()
	}
	return result
}

// Save persists all job states to disk atomically.
func (ss *StateStore) Save() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	sf := stateFile{Jobs: make(map[string]*JobState)}
	for name, jt := range ss.jobs {
		js := jt.Snapshot()
		sf.Jobs[name] = &js
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := ss.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}

	return os.Rename(tmp, ss.path)
}

// JobTracker is a thread-safe view of a single job's state.
type JobTracker struct {
	mu    sync.Mutex
	state JobState
}

// SetRunning updates the running flag.
func (jt *JobTracker) SetRunning(running bool) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.Running = running
}

// SetStatus updates the last status and error.
func (jt *JobTracker) SetStatus(status, errMsg string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.LastStatus = status
	jt.state.LastError = errMsg
	jt.state.LastRun = time.Now()
}

// SetNextRun updates the next scheduled run time.
func (jt *JobTracker) SetNextRun(next time.Time) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.state.NextRun = next
}

// Snapshot returns a copy of the current state.
func (jt *JobTracker) Snapshot() JobState {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	return jt.state
}

// EnsureDir creates the directory for a path if it doesn't exist.
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0755)
}
