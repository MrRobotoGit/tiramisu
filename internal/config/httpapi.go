package config

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
)

// HistoryDeps is what the history routes need from the engine.
type HistoryDeps struct {
	Mu    *sync.Mutex                                // the POST /api/config lock: one save at a time
	Actor func(*http.Request) (source, actor string) // who is asking
	Apply func(Config)                               // put a configuration in use (live reload)
	Logf  func(string, ...any)
	// Allowed says whether r may use these routes: with sign-in on, a browser session only
	// (a rollback can turn HTTPS off or change the trusted proxies). Nil allows all.
	Allowed func(*http.Request) bool
}

func (d HistoryDeps) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Allowed != nil && !d.Allowed(r) {
			http.Error(w, "the configuration history is for the Control Panel, signed in", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// RegisterHistory mounts /api/config/history, /api/config/history/{id} and /api/config/rollback.
func (b *Boot) RegisterHistory(mux *http.ServeMux, d HistoryDeps) {
	mux.HandleFunc("GET /api/config/history", d.guard(func(w http.ResponseWriter, r *http.Request) {
		limit := KeepVersions
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < limit {
			limit = n
		}
		h, err := b.Store.History(limit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, h)
	}))
	mux.HandleFunc("GET /api/config/history/{id}", d.guard(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "id must be a number", 400)
			return
		}
		doc, ok, err := b.Store.Version(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !ok {
			http.Error(w, "no such version (the history keeps the last 50)", 404)
			return
		}
		c, _ := Resolve(doc, Env{})
		writeJSON(w, 200, c.Redacted())
	}))
	mux.HandleFunc("GET /api/config/history/{id}/restore", d.guard(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "id must be a number", 400)
			return
		}
		ch, err := b.RestorePreview(id)
		if errors.Is(err, ErrNoVersion) {
			http.Error(w, "no such version (the history keeps the last 50)", 404)
			return
		} else if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "changed": len(ch) > 0, "changes": ch, "restart": NeedsRestart(ch)})
	}))
	mux.HandleFunc("POST /api/config/rollback", d.guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.ID <= 0 {
			http.Error(w, `the body must be {"id": N}`, 400)
			return
		}
		d.Mu.Lock()
		defer d.Mu.Unlock()
		_, actor := d.Actor(r)
		c, ver, ch, err := b.Rollback(req.ID, actor)
		if errors.Is(err, ErrNoVersion) {
			http.Error(w, "no such version (the history keeps the last 50)", 404)
			return
		} else if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		d.Apply(c)
		changed := len(ch) > 0
		if d.Logf != nil && changed {
			d.Logf("[Config] Rolled back to version #%d as #%d", req.ID, ver)
		}
		writeJSON(w, 200, map[string]any{"id": ver, "changed": changed, "changes": ch, "restart": NeedsRestart(ch)})
	}))
}
