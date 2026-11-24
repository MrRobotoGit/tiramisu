package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactedHidesSecrets(t *testing.T) {
	var c Config
	c.Plex.Token, c.TMDBAPIKey, c.Prowlarr.APIKey = "plex-secret", "tmdb-secret", "prowlarr-secret"
	b, _ := json.Marshal(c.Redacted())
	s := string(b)
	for _, secret := range []string{"plex-secret", "tmdb-secret", "prowlarr-secret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("%s exposed", secret)
		}
	}
	if !strings.Contains(s, `"secrets_configured":{"plex.token":true,"tmdb_api_key":true,"prowlarr.api_key":true}`) {
		t.Fatalf("flags missing: %s", s)
	}
	if c.Plex.Token != "plex-secret" {
		t.Fatal("Redacted modified the original")
	}
	// The panel posts back what it received: decoding must still yield a Config.
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	back.KeepSecrets(&c)
	if back.Plex.Token != "plex-secret" || back.TMDBAPIKey != "tmdb-secret" || back.Prowlarr.APIKey != "prowlarr-secret" {
		t.Fatal("secrets lost on save")
	}
	back.TMDBAPIKey = "new"
	back.KeepSecrets(&c)
	if back.TMDBAPIKey != "new" {
		t.Fatal("a new value must win")
	}
}

func TestSecretsDoNotFollowANewURL(t *testing.T) {
	var old Config
	old.Plex.URL, old.Plex.Token = "http://plex:32400", "plex-secret"
	old.Prowlarr.URL, old.Prowlarr.APIKey = "http://prowlarr:9696", "prowlarr-secret"
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"plex":{"url":"http://attacker:80"}}`, false},
		{`{"plex":{"url":"http://attacker:80","token":"new"}}`, true},
		{`{"prowlarr":{"url":"http://attacker:80"}}`, false},
		{`{"prowlarr":{"url":"http://attacker:80","api_key":"new"}}`, true},
		{`{"plex":{"library_id":3}}`, true},
	} {
		next, err := old.Patch([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if err := next.SecretsFollowURL(&old, []byte(tc.body)); (err == nil) != tc.ok {
			t.Errorf("%s: err %v", tc.body, err)
		}
	}
}

// The audit's bypass: PLEX_TOKEN from the environment, plex.url from the saved document.
func TestEnvPlexTokenDoesNotFollowASavedURL(t *testing.T) {
	b := newBoot(t)
	_, n, err := b.PatchSaved([]byte(`{"plex":{"url":"http://plex:32400"}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, v1, _, _ := b.SaveConfig(n, "ui", "", "")
	_, n, _ = b.PatchSaved([]byte(`{"plex":{"url":"http://elsewhere:32400"}}`))
	b.SaveConfig(n, "ui", "", "")

	t.Setenv("PLEX_TOKEN", "ENV-SECRET")
	for _, body := range []string{`{"plex":{"url":"http://attacker.example"}}`, `{"plex":{"url":"http://attacker.example","token":"mine"}}`} {
		if _, _, err := b.PatchSaved([]byte(body)); err == nil {
			t.Errorf("%s: saved with the environment's token", body)
		}
	}
	if _, _, err := b.PatchSaved([]byte(`{"log_level":"DEBUG"}`)); err != nil {
		t.Fatalf("unrelated change refused: %v", err)
	}
	if _, _, _, err := b.Rollback(v1, ""); err == nil {
		t.Fatal("rollback moved the environment's token to another plex.url")
	}
	t.Setenv("PLEX_URL", "http://plex:32400") // the environment owns both: a saved URL changes nothing
	if _, _, err := b.PatchSaved([]byte(`{"plex":{"url":"http://attacker.example"}}`)); err != nil {
		t.Fatalf("saved URL with PLEX_URL set: %v", err)
	}
}
