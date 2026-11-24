package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// ErrHeld: the job is held while the node's library comes from a master.
var ErrHeld = errors.New("held by the library master")

const msgHeld = "Movies, TV and Watchlist syncs are off while %s sends you its library. Your schedule is kept and comes back when you leave."

// heldJobs are the syncs a library master replaces; music is not part of the shared library.
var heldJobs = map[string]bool{"movies": true, "tv": true, "watchlist": true}

// SetHold makes movies, tv and watchlist wait while hold returns a master's name.
func (s *Scheduler) SetHold(hold func() string) { s.hold.Store(&hold) }

// heldBy is the master holding job name, "" when it may run.
func (s *Scheduler) heldBy(name string) string {
	h := s.hold.Load()
	if h == nil || !heldJobs[name] {
		return ""
	}
	return (*h)()
}

// StopHeld cancels the held jobs that are running (the node just got a master).
func (s *Scheduler) StopHeld() {
	if s == nil {
		return
	}
	for name := range heldJobs {
		if s.heldBy(name) == "" {
			continue
		}
		if err := s.StopJob(name); err == nil {
			log.Printf("[Scheduler] %s held: stopped", name)
		}
	}
}

// WaitHeld returns once no held job runs, or when ctx ends: a job stops between titles, not at once.
func (s *Scheduler) WaitHeld(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for {
		ended := s.endedCh() // taken before the check: an end after it still wakes us
		busy := false
		st := s.state.Status()
		for name := range heldJobs {
			busy = busy || (st[name].Running && s.heldBy(name) != "")
		}
		if !busy {
			return nil
		}
		select {
		case <-ended:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// writeHeld answers a held "Run now" with the 409 the Control Panel reads.
func (s *Scheduler) writeHeld(w http.ResponseWriter, name string) {
	master := s.heldBy(name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]string{"error": "library_from_master", "master": master, "message": fmt.Sprintf(msgHeld, master)})
}
