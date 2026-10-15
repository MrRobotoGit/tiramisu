package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
}

func NewJobs(max int, ttl, timeout time.Duration) *Jobs {
	return &Jobs{jobs: map[string]*Job{}, max: max, ttl: ttl, timeout: timeout}
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
	job := &Job{ID: newID(), Kind: kind, status: StatusRunning, done: make(chan struct{})}
	j.jobs[job.ID] = job
	j.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), j.timeout)
		defer cancel()
		res, err := fn(ctx)
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
