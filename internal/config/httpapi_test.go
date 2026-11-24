package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHistoryRoutes(t *testing.T) {
	b := newBoot(t)
	_, n, _ := b.PatchSaved([]byte(`{"plex":{"token":"secret"},"log_level":"DEBUG"}`))
	_, v1, _, _ := b.SaveConfig(n, "ui", "admin", "")
	_, n, _ = b.PatchSaved([]byte(`{"log_level":"WARN"}`))
	b.SaveConfig(n, "ui", "admin", "")

	var applied *Config
	mux := http.NewServeMux()
	b.RegisterHistory(mux, HistoryDeps{Mu: &sync.Mutex{}, Actor: func(*http.Request) (string, string) { return "ui", "admin" },
		Apply: func(c Config) { applied = &c }})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	rec := do("GET", "/api/config/history?limit=1", "")
	var h []Entry
	if json.Unmarshal(rec.Body.Bytes(), &h); rec.Code != 200 || len(h) != 1 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	rec = do("GET", "/api/config/history/"+itoa64(v1), "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"token":"secret"`) || !strings.Contains(rec.Body.String(), `"plex.token":true`) {
		t.Fatalf("version view must be redacted: %d %s", rec.Code, rec.Body)
	}
	if do("GET", "/api/config/history/999", "").Code != 404 || do("GET", "/api/config/history/x", "").Code != 400 {
		t.Fatal("missing or bad id")
	}
	rec = do("POST", "/api/config/rollback", `{"id": `+itoa64(v1)+`}`)
	if rec.Code != 200 || applied == nil || applied.LogLevel != "DEBUG" {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body)
	}
	if do("POST", "/api/config/rollback", `{"id": 999}`).Code != 404 || do("POST", "/api/config/rollback", `{}`).Code != 400 {
		t.Fatal("rollback errors")
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Audit M2 #4: the routes refuse whatever Allowed refuses (the internal token with sign-in on).
func TestHistoryRoutesGuard(t *testing.T) {
	b := newBoot(t)
	mux := http.NewServeMux()
	b.RegisterHistory(mux, HistoryDeps{Mu: &sync.Mutex{}, Actor: func(*http.Request) (string, string) { return "internal", "" },
		Apply: func(Config) { t.Fatal("applied") }, Allowed: func(*http.Request) bool { return false }})
	for _, rq := range [][2]string{{"GET", "/api/config/history"}, {"GET", "/api/config/history/1"}, {"POST", "/api/config/rollback"}} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(rq[0], rq[1], strings.NewReader(`{"id":1}`)))
		if rec.Code != 403 {
			t.Fatalf("%s %s: %d", rq[0], rq[1], rec.Code)
		}
	}
}

func TestRestorePreviewMatchesRollback(t *testing.T) {
	b := newBoot(t)
	_, n, _ := b.PatchSaved([]byte(`{"plex":{"token":"A"},"log_level":"DEBUG"}`))
	_, v1, _, _ := b.SaveConfig(n, "ui", "", "")
	_, n, _ = b.PatchSaved([]byte(`{"plex":{"token":"B"}}`))
	b.SaveConfig(n, "ui", "", "")
	_, n, _ = b.PatchSaved([]byte(`{"plex":{"token":"A"},"metrics_port":9191}`))
	b.SaveConfig(n, "ui", "", "")
	pre, err := b.RestorePreview(v1)
	if err != nil || len(pre) != 1 || pre[0].Key != "metrics_port" {
		t.Fatalf("preview (secret A→B→A is no change, telemetry kept): %+v %v", pre, err)
	}
	_, _, ch, _ := b.Rollback(v1, "")
	if len(ch) != len(pre) || ch[0] != pre[0] {
		t.Fatalf("rollback %+v != preview %+v", ch, pre)
	}
}
