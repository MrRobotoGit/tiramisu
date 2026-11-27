package config

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Change is one key that differs between two versions. Secrets carry no values.
type Change struct {
	Key    string `json:"key"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

var secretKeys = map[string]bool{"plex.token": true, "tmdb_api_key": true, "prowlarr.api_key": true}

// liveKeys apply without a restart (applyConfig, or read through gc() on every use); every
// other key is captured at start-up by the mount, the syncers or the clients.
var liveKeys = map[string]bool{"stall_notice": true, "blocklist_enabled": true, "blocklist_url": true, "blocklist_filter": true,
	"log_level": true, "telemetry": true}

// Diff flattens both documents to dotted keys (arrays compared whole) and lists what differs.
func Diff(a, b []byte) []Change {
	fa, fb := map[string]string{}, map[string]string{}
	flatten(a, fa)
	flatten(b, fb)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	var out []Change
	for k := range keys {
		if fa[k] == fb[k] {
			continue
		}
		c := Change{Key: k, From: fa[k], To: fb[k]}
		if secretKeys[k] {
			c = Change{Key: k, Secret: true}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// NeedsRestart says whether any of the changes takes effect only after a restart.
func NeedsRestart(changes []Change) bool {
	for _, c := range changes {
		if !liveKeys[c.Key] {
			return true
		}
	}
	return false
}

func flatten(doc []byte, out map[string]string) {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, x)
			}
			return
		}
		if a, ok := v.([]any); ok && len(a) == 0 {
			v = nil // an empty list and no list are the same setting
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(v)
		out[prefix] = strings.TrimSpace(buf.String())
	}
	walk("", v)
}

// Entry is one version in the history, with what it changed from the one before.
type Entry struct {
	ID        int64    `json:"id"`
	CreatedAt int64    `json:"created_at"`
	Source    string   `json:"source"`
	Actor     string   `json:"actor"`
	Note      string   `json:"note"`
	Changes   []Change `json:"changes"`
	Restart   bool     `json:"restart"`
	Current   bool     `json:"current"`
	Baseline  bool     `json:"baseline,omitempty"` // oldest kept after pruning: what it changed is gone
	Kind      string   `json:"kind,omitempty"`     // "engine" for Tentacle settings; empty for the configuration
}

// History lists the newest limit versions; the oldest kept one is compared to the defaults.
func (s *Store) History(limit int) ([]Entry, error) {
	vs, err := s.db.ListConfigVersions(limit + 1)
	if err != nil {
		return nil, err
	}
	def, _ := Document(defaults())
	out := []Entry{}
	for i, v := range vs {
		if i == limit {
			break
		}
		var ch []Change
		baseline := false
		switch {
		case i+1 < len(vs):
			ch = Diff(vs[i+1].JSON, v.JSON)
		case v.ID > 1: // ids are AUTOINCREMENT from 1: anything older was pruned
			baseline = true // comparing to the defaults would list long-standing settings as its changes
		default:
			ch = Diff(def, v.JSON)
		}
		if ch == nil {
			ch = []Change{}
		}
		out = append(out, Entry{ID: v.ID, CreatedAt: v.CreatedAt, Source: v.Source, Actor: v.Actor, Note: v.Note,
			Changes: ch, Restart: NeedsRestart(ch), Current: i == 0, Baseline: baseline})
	}
	return out, nil
}

// Version is one saved document, or ok=false.
func (s *Store) Version(id int64) ([]byte, bool, error) {
	v, ok, err := s.db.ConfigVersion(id)
	return v.JSON, ok, err
}
