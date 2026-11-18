package config

import (
	"encoding/json"
	"fmt"
)

// Patch returns c with the keys of body applied on top: a key left out keeps its value,
// nested objects merge key by key, arrays are replaced whole, and an empty secret keeps
// the stored one. A body that is not a JSON object, or has a field of the wrong type, is
// refused without touching anything. The result is meant to be saved as a document:
// its json:"-" fields are not filled in, so resolve it before using it.
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
	return next, nil
}
