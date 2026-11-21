package settings

import (
	"encoding/json"
	"errors"
	"strings"
)

// document is the stored JSON object with every key kept raw, so the keys Tentacle does
// not interpret (dead or unknown) round-trip unchanged on every write.
type document map[string]json.RawMessage

var errNotObject = errors.New("settings: the document is not a JSON object")

// parseDocument reads a stored document. A key missing from it reads as the zero value
// (a missing boolean is false even when its default is true), as today.
func parseDocument(b []byte) (document, Settings, error) {
	var d document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, Settings{}, err
	}
	if d == nil {
		return nil, Settings{}, errNotObject
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, Settings{}, err
	}
	return d, s, nil
}

// with overlays s on d. A raw key that differs from an interpreted one only by case is
// dropped: the JSON decoder would let it win over the field.
func (d document) with(s Settings) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var known document
	if err := json.Unmarshal(b, &known); err != nil {
		return nil, err
	}
	out := make(document, len(d)+len(known))
	for k, v := range d {
		if !foldsToKnown(k) {
			out[k] = v
		}
	}
	for k, v := range known {
		out[k] = v
	}
	return json.Marshal(out)
}

func foldsToKnown(k string) bool {
	for _, n := range Keys() {
		if strings.EqualFold(k, n) {
			return true
		}
	}
	return false
}
