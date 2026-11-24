package config

import (
	"encoding/json"
	"fmt"
)

// Secrets are write-only over the API (issue #26): GET never returns them, and a
// save that leaves one empty keeps the stored value, unless its URL changes.

// SecretsConfigured reports which secrets have a value, by their JSON path.
type SecretsConfigured struct {
	PlexToken      bool `json:"plex.token"`
	TMDBAPIKey     bool `json:"tmdb_api_key"`
	ProwlarrAPIKey bool `json:"prowlarr.api_key"`
}

// RedactedView is what GET /api/config returns.
type RedactedView struct {
	Config
	SecretsConfigured SecretsConfigured `json:"secrets_configured"`
}

// Redacted returns a copy without secret values.
func (c *Config) Redacted() RedactedView {
	v := RedactedView{Config: *c}
	v.SecretsConfigured = SecretsConfigured{
		PlexToken:      c.Plex.Token != "",
		TMDBAPIKey:     c.TMDBAPIKey != "",
		ProwlarrAPIKey: c.Prowlarr.APIKey != "",
	}
	v.Plex.Token, v.TMDBAPIKey, v.Prowlarr.APIKey = "", "", ""
	return v
}

// KeepSecrets fills the secrets c leaves empty with the ones in old.
func (c *Config) KeepSecrets(old *Config) {
	if c.Plex.Token == "" {
		c.Plex.Token = old.Plex.Token
	}
	if c.TMDBAPIKey == "" {
		c.TMDBAPIKey = old.TMDBAPIKey
	}
	if c.Prowlarr.APIKey == "" {
		c.Prowlarr.APIKey = old.Prowlarr.APIKey
	}
}

// SecretsFollowURL refuses a save that would send a stored secret to a new URL: when
// plex.url or prowlarr.url changes, body must carry the secret again.
func (c *Config) SecretsFollowURL(old *Config, body []byte) error {
	var probe struct {
		Plex struct {
			Token string `json:"token"`
		} `json:"plex"`
		Prowlarr struct {
			APIKey string `json:"api_key"`
		} `json:"prowlarr"`
	}
	_ = json.Unmarshal(body, &probe) // Patch has already refused a malformed body
	if c.Plex.URL != old.Plex.URL {
		if envPlexTokenFollows() {
			return errEnvPlexToken
		}
		if old.Plex.Token != "" && probe.Plex.Token == "" {
			return fmt.Errorf("plex.url changed: enter plex.token again, the stored one is not sent to a new address")
		}
	}
	if c.Prowlarr.URL != old.Prowlarr.URL && old.Prowlarr.APIKey != "" && probe.Prowlarr.APIKey == "" {
		return fmt.Errorf("prowlarr.url changed: enter prowlarr.api_key again, the stored one is not sent to a new address")
	}
	return nil
}

// envPlexTokenFollows: the token comes from the environment, which no save overrides, and
// plex.url does not, so a saved URL change would take the environment's token with it.
func envPlexTokenFollows() bool {
	return firstEnv("TIRAMISU_PLEX_TOKEN", "GOSTREAM_PLEX_TOKEN", "PLEX_TOKEN") != "" &&
		firstEnv("TIRAMISU_PLEX_URL", "GOSTREAM_PLEX_URL", "PLEX_URL") == ""
}

var errEnvPlexToken = fmt.Errorf("plex.token comes from the environment (PLEX_TOKEN): set plex.url there too (PLEX_URL), not here")
