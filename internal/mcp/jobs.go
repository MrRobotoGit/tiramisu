package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Job is one piece of slow work: a search, an add, an inspect.
type Job struct {
	ID       string
	Kind     string
	status   string
	result   any
	err      error
	finished time.Time
	done     chan struct{}
	cancel   context.CancelFunc
}

// Snapshot is a job as seen at one instant.
type Snapshot struct {
	ID     string
	Kind   string
	Status string
	Result any
	Err    error
}

type Jobs struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	max     int
	ttl     time.Duration
	timeout time.Duration
	logger  *log.Logger
}

func NewJobs(max int, ttl, timeout time.Duration) *Jobs {
	return &Jobs{jobs: map[string]*Job{}, max: max, ttl: ttl, timeout: timeout, logger: log.Default()}
}

// Start runs fn in the background with its own deadline; the caller's request may end first.
func (j *Jobs) Start(kind string, fn func(ctx context.Context) (any, error)) (*Job, error) {
	j.mu.Lock()
	j.purgeLocked()
	if len(j.jobs) >= j.max {
		j.evictOldestFinishedLocked()
	}
	if len(j.jobs) >= j.max {
		j.mu.Unlock()
		return nil, fmt.Errorf("too many running jobs (%d): wait for one to finish", j.max)
	}
	ctx, cancel := context.WithTimeout(context.Background(), j.timeout)
	job := &Job{ID: newID(), Kind: kind, status: StatusRunning, done: make(chan struct{}), cancel: cancel}
	j.jobs[job.ID] = job
	j.mu.Unlock()

	go func() {
		defer cancel()
		res, err := run(ctx, j.logger, kind, job.ID, fn)
		j.mu.Lock()
		job.result, job.err, job.finished = res, err, time.Now()
		job.status = StatusDone
		if err != nil {
			job.status = StatusFailed
		}
		j.mu.Unlock()
		close(job.done)
	}()
	return job, nil
}

// run calls fn, turning a panic into the job's error: a bare goroutine that panics
// takes the whole process down, playback included.
func run(ctx context.Context, logger *log.Logger, kind, id string, fn func(ctx context.Context) (any, error)) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("[MCP] job %s %s panicked: %v\n%s", kind, id, r, debug.Stack())
			res, err = nil, fmt.Errorf("internal error in %s: %v", kind, r)
		}
	}()
	return fn(ctx)
}

// ErrNotCancellable refuses to interrupt a job that writes to the library halfway.
var ErrNotCancellable = errors.New("only searches and inspects can be cancelled: an add is left to finish")

// cancellable are the kinds that leave nothing behind when stopped midway; an
// inspect cancelled while waiting for metadata drops the torrent it hydrated.
var cancellable = map[string]bool{"search": true, "search_album": true, "inspect": true}

// Cancel stops a running search or inspect; the job then ends as failed with context.Canceled.
func (j *Jobs) Cancel(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return fmt.Errorf("unknown job_id %q: it expired or tiramisu restarted", id)
	}
	if job.status != StatusRunning {
		return fmt.Errorf("job %s already %s: there is nothing to cancel", id, job.status)
	}
	if !cancellable[job.Kind] {
		return ErrNotCancellable
	}
	job.cancel()
	return nil
}

// Wait blocks up to d for the job to finish, then reports its state.
func (j *Jobs) Wait(ctx context.Context, id string, d time.Duration) (Snapshot, error) {
	j.mu.Lock()
	j.purgeLocked()
	job, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("unknown job_id %q: it expired or tiramisu restarted, start it again", id)
	}
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-job.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return Snapshot{ID: job.ID, Kind: job.Kind, Status: job.status, Result: job.result, Err: job.err}, nil
}

// finished explains why snap cannot feed an add: still running, failed, or another kind.
func finished(snap Snapshot, want string) error {
	switch {
	case snap.Status == StatusRunning:
		return fmt.Errorf("job %s is still running: poll get_job until it is done", snap.ID)
	case snap.Status == StatusFailed:
		return fmt.Errorf("job %s failed, there is nothing to add from it: %v", snap.ID, snap.Err)
	case snap.Kind != want:
		return fmt.Errorf("job %s is a %s, not a %s", snap.ID, snap.Kind, want)
	}
	return nil
}

func (j *Jobs) purgeLocked() {
	now := time.Now()
	for id, job := range j.jobs {
		if job.status != StatusRunning && now.Sub(job.finished) > j.ttl {
			delete(j.jobs, id)
		}
	}
}

// evictOldestFinishedLocked frees one slot: only running jobs may hold the cap.
func (j *Jobs) evictOldestFinishedLocked() {
	var oldest *Job
	for _, job := range j.jobs {
		if job.status != StatusRunning && (oldest == nil || job.finished.Before(oldest.finished)) {
			oldest = job
		}
	}
	if oldest != nil {
		delete(j.jobs, oldest.ID)
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
