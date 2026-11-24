package scheduler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ServeStatus answers GET /api/scheduler/status.
func (s *Scheduler) ServeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Status())
}

// ServeAction answers POST /api/scheduler/{job}/run and /stop; any other shape is 404.
func (s *Scheduler) ServeAction(w http.ResponseWriter, r *http.Request) {
	name, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/scheduler/"), "/")
	if !ok || name == "" || (action != "run" && action != "stop") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var err error
	switch action {
	case "run":
		if err = s.TriggerRun(name); errors.Is(err, ErrHeld) {
			s.writeHeld(w, name)
			return
		}
	case "stop":
		err = s.StopJob(name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
