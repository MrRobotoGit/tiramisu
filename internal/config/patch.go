package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Patch returns c with the keys of body applied on top: a key left out keeps its value,
// nested objects merge key by key, arrays are replaced whole, and an empty secret keeps
// the stored one. A body that is not a JSON object, or has a field of the wrong type, is
// refused without touching anything. The result is meant to be written to config.json:
// its json:"-" fields are not filled in, so reload it with LoadConfig before using it.
func (c *Config) Patch(body []byte) (Config, error) {
	var next Config
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return next, fmt.Errorf("the body must be a JSON object: %v", err)
	}
	if probe == nil {
		return next, fmt.Errorf("the body must be a JSON object, not null")
	}
	// A deep copy through JSON: the decode below must not write into c's slices.
	base, err := json.Marshal(c)
	if err != nil {
		return next, err
	}
	if err := json.Unmarshal(base, &next); err != nil {
		return next, err
	}
	// An unset scoring profile means "defaults": a patch of one weight starts from them,
	// not from a profile of zeros.
	if raw, ok := probe["quality_scoring"]; ok {
		var qs map[string]json.RawMessage
		if json.Unmarshal(raw, &qs) == nil {
			if _, ok := qs["movies"]; ok && next.QualityScoringConfig.Movies == nil {
				d := DefaultMovieWeights()
				next.QualityScoringConfig.Movies = &d
			}
			if _, ok := qs["tv"]; ok && next.QualityScoringConfig.TV == nil {
				d := DefaultTVWeights()
				next.QualityScoringConfig.TV = &d
			}
		}
	}
	if err := json.Unmarshal(body, &next); err != nil {
		return Config{}, err
	}
	next.KeepSecrets(c)
	next.ConfigPath = c.ConfigPath // where it gets written
	return next, nil
}

// WriteFile writes c to path through a temporary file and a rename, mode 0600: a reader
// never sees half a file, and a failed write leaves the old one in place.
func (c *Config) WriteFile(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
