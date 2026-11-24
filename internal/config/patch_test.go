package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPatchKeepsWhatTheBodyLeavesOut(t *testing.T) {
	cur := Config{MetricsPort: 9080, LogLevel: "INFO", TMDBAPIKey: "k", ConfigPath: "/x/config.json"}
	cur.Plex.URL, cur.Plex.Token = "http://plex:32400", "secret"
	cur.AuthTrustedProxies = []string{"10.0.0.1"}

	next, err := cur.Patch([]byte(`{"log_level":"DEBUG","plex":{"url":"http://new:32400"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.LogLevel != "DEBUG" || next.MetricsPort != 9080 || next.Plex.URL != "http://new:32400" {
		t.Fatalf("patch not applied as a merge: %+v", next)
	}
	if next.Plex.Token != "secret" || next.TMDBAPIKey != "k" {
		t.Fatal("secrets lost")
	}
	// An empty secret keeps the stored one, as with the Control Panel.
	next, _ = cur.Patch([]byte(`{"tmdb_api_key":""}`))
	if next.TMDBAPIKey != "k" {
		t.Fatal("empty secret cleared the stored one")
	}
	// Arrays are replaced whole, and c is never written into.
	next, _ = cur.Patch([]byte(`{"auth_trusted_proxies":["10.0.0.2"]}`))
	if len(next.AuthTrustedProxies) != 1 || next.AuthTrustedProxies[0] != "10.0.0.2" || cur.AuthTrustedProxies[0] != "10.0.0.1" {
		t.Fatalf("array merge wrong or source mutated: %v / %v", next.AuthTrustedProxies, cur.AuthTrustedProxies)
	}
}

func TestPatchRefusesBadBodies(t *testing.T) {
	cur := Config{MetricsPort: 9080}
	for _, b := range []string{``, `null`, `[]`, `"x"`, `{"metrics_port":"9080"}`, `{bad`} {
		if _, err := cur.Patch([]byte(b)); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
}

// The full object GET returns, sent back unchanged, must not change anything.
func TestPatchRoundTripOfRedactedView(t *testing.T) {
	cur := Config{MetricsPort: 9080, TMDBAPIKey: "k"}
	cur.Plex.Token = "secret"
	body, _ := json.Marshal(cur.Redacted())
	next, err := cur.Patch(body)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(cur)
	b, _ := json.Marshal(next)
	if string(a) != string(b) {
		t.Fatalf("round trip changed the config:\n%s\n%s", a, b)
	}
}

// An unset profile means defaults: patching one weight must not zero the others.
func TestPatchScoringStartsFromDefaults(t *testing.T) {
	cur := Config{}
	next, err := cur.Patch([]byte(`{"quality_scoring":{"movies":{"res_4k":5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := next.QualityScoringConfig.Movies
	d := DefaultMovieWeights()
	if m == nil || m.Res4K != 5 || m.MinSeeders != d.MinSeeders || m.Max4KGB != d.Max4KGB {
		t.Fatalf("profile = %+v", m)
	}
	if next.QualityScoringConfig.TV != nil {
		t.Fatal("tv profile created although the body did not name it")
	}
	if cur.QualityScoringConfig.Movies != nil {
		t.Fatal("source mutated")
	}
	if _, err := cur.Patch([]byte(`null`)); err == nil || err.Error() == "" {
		t.Fatal("null accepted")
	}
}

func TestWriteAtomicIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"metrics_port":1}`), 0644)
	c := Config{MetricsPort: 9080}
	doc, _ := Document(c)
	if err := writeAtomic(path, doc); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	var back Config
	if json.Unmarshal(data, &back) != nil || back.MetricsPort != 9080 || fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v, content %s", fi.Mode().Perm(), data)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if err := writeAtomic(filepath.Join(dir, "missing", "config.json"), doc); err == nil {
		t.Fatal("write into a missing dir succeeded")
	}
}
