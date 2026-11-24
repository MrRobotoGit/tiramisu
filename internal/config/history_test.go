package config

import (
	"errors"
	"testing"
)

func TestDiff(t *testing.T) {
	a := []byte(`{"plex":{"url":"http://a","token":"s1"},"https":{"extra_hostnames":["x"]},"log_level":"INFO"}`)
	b := []byte(`{"plex":{"url":"http://b","token":"s2"},"https":{"extra_hostnames":["x","y"]},"log_level":"INFO"}`)
	ch := Diff(a, b)
	if len(ch) != 3 {
		t.Fatalf("changes: %+v", ch)
	}
	if ch[0].Key != "https.extra_hostnames" || ch[0].To != `["x","y"]` {
		t.Fatalf("array compared whole: %+v", ch[0])
	}
	if ch[1].Key != "plex.token" || !ch[1].Secret || ch[1].From != "" || ch[1].To != "" {
		t.Fatalf("secret must carry no value: %+v", ch[1])
	}
	if ch[2].Key != "plex.url" || ch[2].From != `"http://a"` {
		t.Fatalf("nested: %+v", ch[2])
	}
	if !NeedsRestart(ch) || NeedsRestart([]Change{{Key: "log_level"}}) {
		t.Fatal("restart detection")
	}
}

func TestHistoryAndRollback(t *testing.T) {
	b := newBoot(t)
	_, n1, _ := b.PatchSaved([]byte(`{"log_level":"DEBUG"}`))
	_, v1, _, _ := b.SaveConfig(n1, "ui", "admin", "")
	_, n2, _ := b.PatchSaved([]byte(`{"metrics_port": 9191}`))
	_, v2, _, _ := b.SaveConfig(n2, "api", "Agent", "")

	h, err := b.Store.History(50)
	if err != nil || h[0].ID != v2 || !h[0].Current || !h[0].Restart || h[0].Actor != "Agent" {
		t.Fatalf("history head: %+v %v", h[0], err)
	}
	if len(h[0].Changes) != 1 || h[0].Changes[0].Key != "metrics_port" {
		t.Fatalf("changes against the previous version: %+v", h[0].Changes)
	}
	oldest := h[len(h)-1]
	if oldest.Changes == nil {
		t.Fatal("changes is always a list")
	}

	c, v3, ch, err := b.Rollback(v1, "admin")
	if err != nil || c.MetricsPort == 9191 || c.LogLevel != "DEBUG" || v3 <= v2 {
		t.Fatalf("rollback: port=%d level=%s v3=%d err=%v", c.MetricsPort, c.LogLevel, v3, err)
	}
	if len(ch) != 1 || ch[0].Key != "metrics_port" {
		t.Fatalf("rollback changes: %+v", ch)
	}
	h, _ = b.Store.History(50)
	if h[0].Source != "rollback" || h[0].Note == "" || len(h) < 4 {
		t.Fatalf("rollback is a new version, nothing deleted: %+v", h[0])
	}
	if _, _, _, err := b.Rollback(99999, "admin"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("missing version: %v", err)
	}
}

// Audit M2 #1, #5: restart is the rule; values are shown unescaped; null and [] are equal.
func TestDiffValuesAndRestartRule(t *testing.T) {
	ch := Diff([]byte(`{"blocklist_url":"a","language":{"preferred_terms":null}}`),
		[]byte(`{"blocklist_url":"a?x=1&y=<2>","language":{"preferred_terms":[]}}`))
	if len(ch) != 1 || ch[0].To != `"a?x=1&y=<2>"` {
		t.Fatalf("diff: %+v", ch)
	}
	if NeedsRestart(ch) {
		t.Fatal("the blocklist applies live")
	}
	for _, k := range []string{"plex.token", "quality_scoring.movies.res_4k", "natpmp.enabled", "master_concurrency_limit"} {
		if !NeedsRestart([]Change{{Key: k}}) {
			t.Fatalf("%s is read at start-up", k)
		}
	}
}

// Audit M2 #2: rolling back to the first version keeps the install's telemetry id.
func TestRollbackKeepsTelemetryID(t *testing.T) {
	b := newBoot(t)
	h, _ := b.Store.History(50)
	first := h[len(h)-1].ID
	cur, _, _ := b.InUse()
	c, _, _, err := b.Rollback(first, "admin")
	if err != nil || cur.TelemetryID == "" || c.TelemetryID != cur.TelemetryID {
		t.Fatalf("telemetry id %q -> %q (%v)", cur.TelemetryID, c.TelemetryID, err)
	}
}

// Audit M2 #3: after pruning, the oldest kept version is a baseline, not a diff from defaults.
func TestHistoryBaselineAfterPruning(t *testing.T) {
	b := newBoot(t)
	_, n, _ := b.PatchSaved([]byte(`{"plex":{"url":"http://x"}}`))
	b.SaveConfig(n, "ui", "", "")
	for i := 0; i < KeepVersions+3; i++ {
		_, n, _ := b.PatchSaved([]byte(`{"metrics_port":` + itoa64(int64(9100+i)) + `}`))
		b.SaveConfig(n, "ui", "", "")
	}
	h, _ := b.Store.History(KeepVersions)
	last := h[len(h)-1]
	if !last.Baseline || len(last.Changes) != 0 {
		t.Fatalf("oldest kept: %+v", last)
	}
}
