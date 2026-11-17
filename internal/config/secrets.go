package config

// Secrets are write-only over the API (issue #26): GET never returns them, and a
// save that leaves one empty keeps the stored value.

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
