package config

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
)

// EngineHistoryDeps is what the Tentacle settings history needs from the engine.
type EngineHistoryDeps struct {
	Actor   func(*http.Request) (source, actor string)
	Allowed func(*http.Request) bool // as HistoryDeps.Allowed
	Logf    func(string, ...any)
	Ignored func() []string // keys that are not manual settings
	Current func() []byte   // the settings in use, as JSON
	// Restore puts a version in use and records it; changed are the keys the engine took.
	Restore func(doc []byte, actor, note string) (changed []string, err error)
	// NeedsRestart reports a key that takes effect only at the next start (UseDisk, TorrentsSavePath).
	NeedsRestart func(key string) bool
}

func (d EngineHistoryDeps) restart(ch []Change) bool {
	for _, c := range ch {
		if d.NeedsRestart != nil && d.NeedsRestart(c.Key) {
			return true
		}
	}
	return false
}

func (d EngineHistoryDeps) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Allowed != nil && !d.Allowed(r) {
			http.Error(w, "the Tentacle settings history is for the Control Panel, signed in", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// diffIgnoring is Diff without the keys that are not manual settings.
func diffIgnoring(a, b []byte, ignored []string) []Change {
	skip := map[string]bool{}
	for _, k := range ignored {
		skip[k] = true
	}
	out := []Change{}
	for _, c := range Diff(a, b) {
		if !skip[c.Key] {
			out = append(out, c)
		}
	}
	return out
}

// EngineHistory lists the newest limit Engine settings versions with what each changed.
func (b *Boot) EngineHistory(limit int, ignored []string, current []byte) ([]Entry, error) {
	vs, err := b.DB.ListGoStormSettingsVersions(limit + 1)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for i, v := range vs {
		if i == limit {
			break
		}
		ch := []Change{}
		baseline := false
		if i+1 < len(vs) {
			ch = diffIgnoring(vs[i+1].JSON, v.JSON, ignored)
		} else if v.Note != "baseline" {
			baseline = true // older versions were pruned: what it changed is gone
		}
		out = append(out, Entry{ID: v.ID, CreatedAt: v.CreatedAt, Source: v.Source, Actor: v.Actor, Note: v.Note,
			Changes: ch, Current: i == 0 && len(diffIgnoring(v.JSON, current, ignored)) == 0,
			Baseline: baseline, Kind: "engine"})
	}
	return out, nil
}

// RegisterEngineHistory mounts /api/tentacle/history, /api/tentacle/history/{id}/restore and
// /api/tentacle/rollback, and the same under /api/gostorm (undocumented aliases). Most engine
// settings apply live or reconnect the client; UseDisk and TorrentsSavePath wait for a restart.
func (b *Boot) RegisterEngineHistory(mux *http.ServeMux, d EngineHistoryDeps) {
	var mu sync.Mutex
	version := func(w http.ResponseWriter, id int64) ([]byte, bool) {
		v, ok, err := b.DB.GoStormSettingsVersion(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return nil, false
		}
		if !ok {
			http.Error(w, "no such version (the history keeps the last 50)", 404)
			return nil, false
		}
		return v.JSON, true
	}
	history := d.guard(func(w http.ResponseWriter, r *http.Request) {
		limit := KeepVersions
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < limit {
			limit = n
		}
		h, err := b.EngineHistory(limit, d.Ignored(), d.Current())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for i := range h {
			h[i].Restart = d.restart(h[i].Changes)
		}
		writeJSON(w, 200, h)
	})
	preview := d.guard(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "id must be a number", 400)
			return
		}
		doc, ok := version(w, id)
		if !ok {
			return
		}
		ch := diffIgnoring(d.Current(), doc, d.Ignored())
		writeJSON(w, 200, map[string]any{"id": id, "changed": len(ch) > 0, "changes": ch, "restart": d.restart(ch)})
	})
	rollback := d.guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.ID <= 0 {
			http.Error(w, `the body must be {"id": N}`, 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		doc, ok := version(w, req.ID)
		if !ok {
			return
		}
		ch := diffIgnoring(d.Current(), doc, d.Ignored())
		if len(ch) == 0 { // applying would only reconnect the client and stop playback
			writeJSON(w, 200, map[string]any{"changed": false, "changes": ch, "restart": false})
			return
		}
		_, actor := d.Actor(r)
		changed, err := d.Restore(doc, actor, "rollback to #"+strconv.FormatInt(req.ID, 10))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if d.Logf != nil {
			d.Logf("[Tentacle] settings restored to version #%d (changed %v)", req.ID, changed)
		}
		// A difference in a key the engine does not interpret changes nothing (plan 2c).
		writeJSON(w, 200, map[string]any{"changed": len(changed) > 0, "changes": ch, "restart": d.restart(ch)})
	})
	mux.HandleFunc("GET /api/tentacle/history", history)
	mux.HandleFunc("GET /api/tentacle/history/{id}/restore", preview)
	mux.HandleFunc("POST /api/tentacle/rollback", rollback)
	mux.HandleFunc("GET /api/gostorm/history", history)
	mux.HandleFunc("GET /api/gostorm/history/{id}/restore", preview)
	mux.HandleFunc("POST /api/gostorm/rollback", rollback)
}
