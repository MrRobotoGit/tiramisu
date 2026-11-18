package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Env is what the configuration in use derives from besides the saved document.
type Env struct {
	RootPath   string // from --path
	LegacyPath string // config.json path: ConfigPath and the default LogDir derive from it, as before
	WithEnv    bool   // apply environment overrides
}

// ParseDocument reads a config.json body: // comments are stripped and the result is the
// canonical document. A body that is not a JSON object is an error.
func ParseDocument(data []byte) ([]byte, error) {
	clean := stripJSONComments(data)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(clean, &probe); err != nil {
		return nil, err
	}
	if probe == nil {
		return nil, errors.New("not a JSON object")
	}
	return Canonical(clean)
}

// ParseDocumentLenient is ParseDocument as the old loader read config.json: a field of the
// wrong type keeps its default and is reported in skipped, the rest is kept.
func ParseDocumentLenient(data []byte) (doc []byte, skipped, err error) {
	clean := stripJSONComments(data)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(clean, &probe); err != nil {
		return nil, nil, err
	}
	if probe == nil {
		return nil, nil, errors.New("not a JSON object")
	}
	cfg, err := applyDocument(clean)
	var te *json.UnmarshalTypeError
	if err != nil && !errors.As(err, &te) {
		return nil, nil, err
	}
	if err != nil {
		skipped = fmt.Errorf("field %s has the wrong type (%s, expected %s)", te.Field, te.Value, te.Type)
	}
	doc, err = Document(cfg)
	return doc, skipped, err
}

// applyDocument decodes doc over the defaults, with the legacy torrserver_url fallback.
// On a type error cfg holds every other field, as json.Unmarshal leaves it.
func applyDocument(doc []byte) (Config, error) {
	cfg := defaults()
	if err := json.Unmarshal(doc, &cfg); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return cfg, err
		}
		applyLegacyURL(&cfg, doc)
		return cfg, err
	}
	applyLegacyURL(&cfg, doc)
	return cfg, nil
}

func applyLegacyURL(cfg *Config, doc []byte) {
	// Backward compat: if gostorm_url was not present in config, fall back to legacy torrserver_url key
	if cfg.GoStormBaseURL == "" {
		var raw map[string]json.RawMessage
		if json.Unmarshal(doc, &raw) == nil {
			if v, ok := raw["torrserver_url"]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					cfg.GoStormBaseURL = s
					log.Printf("[Config] Loaded GoStormBaseURL from legacy key 'torrserver_url': %s", s)
				}
			}
		}
	}
}

// Canonical is doc as stored: defaults filled in, keys spelled as the struct tags.
func Canonical(doc []byte) ([]byte, error) {
	cfg, err := applyDocument(doc)
	if err != nil {
		return nil, err
	}
	return Document(cfg)
}

// Document is what gets stored for c: its JSON fields, never the derived ones.
func Document(c Config) ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// Resolve builds the configuration in use: defaults ← doc ← (env) ← derived. Never writes.
func Resolve(doc []byte, e Env) (Config, error) {
	cfg, err := applyDocument(doc)
	if err != nil {
		return defaults(), err
	}
	cfg.ConfigPath = e.LegacyPath
	cfg.RootPath = e.RootPath
	if e.WithEnv {
		cfg.applyEnvOverrides()
	}
	cfg.finalize()
	cfg.EnableStateDB = true // the configuration lives in the DB: it cannot be off
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(filepath.Dir(cfg.ConfigPath), "logs")
	}
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)
	scriptsDir := filepath.Join(binDir, "scripts")
	logsDir := filepath.Join(binDir, "logs")
	cfg.EngineScripts = map[string]EngineConfig{
		"movies":    {ScriptPath: filepath.Join(scriptsDir, "gostorm-sync-complete.py"), LogsDir: logsDir},
		"tv":        {ScriptPath: filepath.Join(scriptsDir, "gostorm-tv-sync.py"), LogsDir: logsDir},
		"watchlist": {ScriptPath: filepath.Join(scriptsDir, "plex-watchlist-sync.py"), LogsDir: logsDir},
	}
	return cfg, nil
}
