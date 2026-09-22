package musicimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Status is what happened to one album.
type Status string

const (
	StatusPending        Status = "pending"
	StatusNoIdentity     Status = "no-identity"
	StatusAlreadyPresent Status = "already-present"
	StatusNoMatch        Status = "no-match"
	StatusSelected       Status = "selected"
	StatusApplied        Status = "applied"
	StatusError          Status = "error"
)

// Entry is what the run knows about one album. It is the cache that makes a
// resumed run cheap: MusicBrainz and Prowlarr are never asked twice for the same
// answer unless the entry failed.
type Entry struct {
	Artist         string    `json:"artist"`
	Title          string    `json:"title"`
	ReleaseID      string    `json:"release_id,omitempty"`
	ReleaseGroupID string    `json:"release_group_id,omitempty"`
	TorrentHash    string    `json:"torrent_hash,omitempty"`
	TorrentTitle   string    `json:"torrent_title,omitempty"`
	Seeders        int       `json:"seeders,omitempty"`
	Status         Status    `json:"status"`
	Reason         string    `json:"reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// State is the run's durable memory, keyed by Plex rating key.
type State struct {
	Version int              `json:"version"`
	Albums  map[string]Entry `json:"albums"`

	path string
}

func LoadState(path string) (*State, error) {
	state := &State{Version: 1, Albums: map[string]Entry{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, err
	}
	if state.Albums == nil {
		state.Albums = map[string]Entry{}
	}
	state.path = path
	return state, nil
}

// Save writes the state atomically: a crash mid-run must not truncate the cache.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.Rename(temp, s.path)
}

// Get returns the entry for an album, whether or not one exists.
func (s *State) Get(ratingKey string) (Entry, bool) {
	entry, ok := s.Albums[ratingKey]
	return entry, ok
}

// Set records an album's outcome and stamps it.
func (s *State) Set(ratingKey string, entry Entry) {
	entry.UpdatedAt = time.Now()
	s.Albums[ratingKey] = entry
}
