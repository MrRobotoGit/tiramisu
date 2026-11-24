package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tiramisu/internal/metadb"
)

func TestEngineHistoryRoutes(t *testing.T) {
	b := newBoot(t)
	add := func(js, source, note string) int64 {
		id, err := b.DB.InsertGoStormSettingsVersion(metadb.ConfigVersion{JSON: []byte(js), CreatedAt: 1, Source: source, Actor: "admin", Note: note}, 50)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	v1 := add(`{"ConnectionsLimit":25,"PeersListenPort":64407,"StoreSettingsInJson":false}`, "default", "baseline")
	v2 := add(`{"ConnectionsLimit":26,"PeersListenPort":51000,"StoreSettingsInJson":true}`, "ui", "")
	inUse := `{"ConnectionsLimit":26,"PeersListenPort":52000,"StoreSettingsInJson":true}`

	var restored struct {
		doc         string
		actor, note string
	}
	ignored := []string{"StoreSettingsInJson", "PeersListenPort"}
	restoredKeys := []string{"ConnectionsLimit"}
	mux := http.NewServeMux()
	b.RegisterEngineHistory(mux, EngineHistoryDeps{
		Actor:   func(*http.Request) (string, string) { return "ui", "admin" },
		Ignored: func() []string { return ignored },
		Current: func() []byte { return []byte(inUse) },
		Restore: func(doc []byte, actor, note string) ([]string, error) {
			restored.doc, restored.actor, restored.note = string(doc), actor, note
			return restoredKeys, nil
		},
	})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	rec := do("GET", "/api/tentacle/history", "")
	var h []Entry
	if json.Unmarshal(rec.Body.Bytes(), &h); rec.Code != 200 || len(h) != 2 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if h[0].ID != v2 || h[0].Kind != "engine" || h[0].Restart || !h[0].Current {
		t.Fatalf("newest entry: %+v", h[0])
	}
	if len(h[0].Changes) != 1 || h[0].Changes[0].Key != "ConnectionsLimit" || h[0].Changes[0].From != "25" || h[0].Changes[0].To != "26" {
		t.Fatalf("ignored keys must not appear as changes: %+v", h[0].Changes)
	}
	if h[1].ID != v1 || h[1].Note != "baseline" || len(h[1].Changes) != 0 || h[1].Baseline {
		t.Fatalf("baseline entry: %+v", h[1])
	}

	rec = do("GET", "/api/tentacle/history/"+itoa64(v1)+"/restore", "")
	var p struct {
		Changed bool     `json:"changed"`
		Changes []Change `json:"changes"`
		Restart bool     `json:"restart"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &p); rec.Code != 200 || !p.Changed || p.Restart || len(p.Changes) != 1 || p.Changes[0].To != "25" {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if do("GET", "/api/tentacle/history/999/restore", "").Code != 404 {
		t.Fatal("missing version")
	}

	rec = do("POST", "/api/tentacle/rollback", `{"id":`+itoa64(v1)+`}`)
	if rec.Code != 200 || restored.actor != "admin" || restored.note != "rollback to #"+itoa64(v1) || !strings.Contains(restored.doc, `"ConnectionsLimit":25`) || !strings.Contains(rec.Body.String(), `"changed":true`) {
		t.Fatalf("rollback: %d %s %+v", rec.Code, rec.Body, restored)
	}
	// The document differs, but in no key the engine interprets: nothing changed.
	restoredKeys = nil
	if rec = do("POST", "/api/tentacle/rollback", `{"id":`+itoa64(v1)+`}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Fatalf("rollback of dead keys only: %s", rec.Body)
	}
	if do("POST", "/api/tentacle/rollback", `{}`).Code != 400 || do("POST", "/api/tentacle/rollback", `{"id":999}`).Code != 404 {
		t.Fatal("bad rollback bodies")
	}
	// The old /api/gostorm routes stay as aliases.
	if a, b := do("GET", "/api/gostorm/history", ""), do("GET", "/api/tentacle/history", ""); a.Code != 200 || a.Body.String() != b.Body.String() {
		t.Fatalf("alias history: %d %s", a.Code, a.Body)
	}
	if do("GET", "/api/gostorm/history/"+itoa64(v1)+"/restore", "").Code != 200 || do("POST", "/api/gostorm/rollback", `{}`).Code != 400 {
		t.Fatal("alias restore/rollback")
	}
}

func TestEngineHistoryGuard(t *testing.T) {
	b := newBoot(t)
	mux := http.NewServeMux()
	b.RegisterEngineHistory(mux, EngineHistoryDeps{Allowed: func(*http.Request) bool { return false },
		Ignored: func() []string { return nil }, Current: func() []byte { return []byte(`{}`) }})
	for _, c := range [][2]string{{"GET", "/api/gostorm/history"}, {"GET", "/api/gostorm/history/1/restore"}, {"POST", "/api/gostorm/rollback"},
		{"GET", "/api/tentacle/history"}, {"GET", "/api/tentacle/history/1/restore"}, {"POST", "/api/tentacle/rollback"}} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c[0], c[1], strings.NewReader(`{"id":1}`)))
		if rec.Code != 403 {
			t.Fatalf("%s %s: %d", c[0], c[1], rec.Code)
		}
	}
}

// Restoring a version equal to the settings in use must not reconnect the client.
func TestEngineRollbackNoop(t *testing.T) {
	b := newBoot(t)
	id, _ := b.DB.InsertGoStormSettingsVersion(metadb.ConfigVersion{JSON: []byte(`{"ConnectionsLimit":25}`), CreatedAt: 1, Source: "ui"}, 50)
	calls := 0
	mux := http.NewServeMux()
	b.RegisterEngineHistory(mux, EngineHistoryDeps{
		Actor: func(*http.Request) (string, string) { return "ui", "" }, Ignored: func() []string { return nil },
		Current: func() []byte { return []byte(`{"ConnectionsLimit":25}`) },
		Restore: func([]byte, string, string) ([]string, error) { calls++; return nil, nil },
		Allowed: func(*http.Request) bool { return true }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/gostorm/rollback", strings.NewReader(`{"id":`+itoa64(id)+`}`)))
	if rec.Code != 200 || calls != 0 || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Fatalf("no-op rollback: %d %s, restore called %d times", rec.Code, rec.Body, calls)
	}
}

// UseDisk and TorrentsSavePath take effect at the next start: preview and rollback say so.
func TestEngineRollbackReportsRestart(t *testing.T) {
	b := newBoot(t)
	id, err := b.DB.InsertGoStormSettingsVersion(metadb.ConfigVersion{JSON: []byte(`{"UseDisk":true,"ConnectionsLimit":25}`), CreatedAt: 1, Source: "ui", Actor: "admin"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.RegisterEngineHistory(mux, EngineHistoryDeps{
		Actor:        func(*http.Request) (string, string) { return "ui", "admin" },
		Ignored:      func() []string { return nil },
		Current:      func() []byte { return []byte(`{"UseDisk":false,"ConnectionsLimit":25}`) },
		Restore:      func([]byte, string, string) ([]string, error) { return []string{"UseDisk"}, nil },
		NeedsRestart: func(k string) bool { return k == "UseDisk" },
	})
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/tentacle/history/" + itoa64(id) + "/restore", ""},
		{"POST", "/api/tentacle/rollback", `{"id":` + itoa64(id) + `}`},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"restart":true`) {
			t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}
