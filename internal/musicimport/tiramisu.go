package musicimport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SourceFile is one file the engine reports for a torrent.
type SourceFile struct {
	SourcePath string `json:"source_path"`
	FileIndex  int    `json:"file_index"`
	Size       int64  `json:"size"`
	// CueTracks are the tracks a cue sheet in the torrent cuts this file into: a
	// single-file album image. Empty for an ordinary track file.
	CueTracks []CueTrack `json:"-"`
}

// CueTrack is one track of an image, numbered as its cue sheet numbers it.
type CueTrack struct {
	SourcePath string `json:"source_path"`
	Track      int    `json:"track"`
	Title      string `json:"title"`
	Performer  string `json:"performer"`
}

// AddFile is one projection in an add request.
type AddFile struct {
	SourcePath          string `json:"source_path"`
	Path                string `json:"path"`
	ExternalID          string `json:"external_id,omitempty"`
	ExternalIDNamespace string `json:"external_id_ns,omitempty"`
	// CueTrack files one track of an image (see SourceFile.CueTracks); Tags become
	// that track's Vorbis comments.
	CueTrack int               `json:"cue_track,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// AddResult is the engine's answer to an add.
type AddResult struct {
	AlreadyPresent bool `json:"already_present"`
	Files          []struct {
		State string `json:"state"`
		Path  string `json:"path"`
	} `json:"files"`
}

// Tiramisu talks to the Library API.
type Tiramisu struct {
	baseURL string
	http    *http.Client
}

func NewTiramisu(baseURL string) *Tiramisu {
	return &Tiramisu{
		baseURL: strings.TrimRight(baseURL, "/"),
		// An add that cuts a box set from a single-file image runs past five
		// minutes on a slow swarm; the engine bounds its own work.
		http: &http.Client{Timeout: 20 * time.Minute},
	}
}

// CommittedSet is what the library already holds: the external identities, and the
// prefixes derived from the paths carrying the identities found under them. An album
// registered per track has no single album id any more, so the presence of an album
// is a path question.
type CommittedSet struct {
	ExternalIDs   map[string]bool
	AlbumPrefixes map[string]map[string]bool // prefix -> external ids under it
}

// HasAlbum reports whether the album is already committed. The artist-qualified
// prefix is enough on its own; the bare-name fallback exists for releases added by
// hand as a single folder, and it only counts when the identities under that folder
// include this album's: a same-named album by another artist must not hide a
// missing import.
func (c CommittedSet) HasAlbum(artist, album, identity string) bool {
	prefix := strings.Trim(sanitizeComponent(artist)+"/"+sanitizeComponent(album), "/")
	if len(c.AlbumPrefixes[prefix]) > 0 {
		return true
	}
	bare := c.AlbumPrefixes[sanitizeComponent(album)]
	if len(bare) == 0 {
		return false
	}
	return identity == "" || bare[identity]
}

// Committed reads every committed projection once.
func (t *Tiramisu) Committed(ctx context.Context) (CommittedSet, error) {
	set := CommittedSet{ExternalIDs: map[string]bool{}, AlbumPrefixes: map[string]map[string]bool{}}
	cursor := ""
	for {
		query := url.Values{"type": {"music"}, "limit": {"500"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Items []struct {
				Path                string `json:"path"`
				ExternalID          string `json:"external_id"`
				ExternalIDNamespace string `json:"external_id_ns"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := t.do(ctx, http.MethodGet, "/api/library/list?"+query.Encode(), nil, &page); err != nil {
			return set, err
		}
		for _, item := range page.Items {
			if item.ExternalID != "" {
				set.ExternalIDs[item.ExternalID] = true
			}
			parts := strings.Split(item.Path, "/")
			record := func(prefix string) {
				if prefix == "" {
					return
				}
				ids := set.AlbumPrefixes[prefix]
				if ids == nil {
					ids = map[string]bool{}
					set.AlbumPrefixes[prefix] = ids
				}
				if item.ExternalID != "" {
					ids[item.ExternalID] = true
				}
			}
			record(parts[0])
			if len(parts) > 1 {
				record(parts[0] + "/" + parts[1])
			}
		}
		if page.NextCursor == "" {
			return set, nil
		}
		cursor = page.NextCursor
	}
}

// Inspect hydrates the torrent if needed and returns its file list, each image
// carrying the cue tracks it is cut into.
func (t *Tiramisu) Inspect(ctx context.Context, hash, title string, torrentFile []byte) ([]SourceFile, error) {
	var response struct {
		Files     []SourceFile `json:"files"`
		CueTracks []CueTrack   `json:"cue_tracks"`
	}
	payload := map[string]interface{}{"hash": hash, "title": title}
	if strings.HasPrefix(hash, "magnet:") {
		payload = map[string]interface{}{"magnet": hash, "title": title}
	}
	if len(torrentFile) > 0 {
		payload["torrent_file"] = torrentFile
	}
	if err := t.do(ctx, http.MethodPost, "/api/library/inspect", payload, &response); err != nil {
		return nil, err
	}
	for i := range response.Files {
		for _, track := range response.CueTracks {
			if track.SourcePath == response.Files[i].SourcePath {
				response.Files[i].CueTracks = append(response.Files[i].CueTracks, track)
			}
		}
	}
	return response.Files, nil
}

// RemovePath removes one projection by its section-relative path.
func (t *Tiramisu) RemovePath(ctx context.Context, path string) error {
	return t.do(ctx, http.MethodPost, "/api/library/remove", map[string]string{"type": "music", "path": path}, nil)
}

// Add files an album's projections.
func (t *Tiramisu) Add(ctx context.Context, hash, title string, torrentFile []byte, files []AddFile) (AddResult, error) {
	var result AddResult
	payload := map[string]interface{}{"type": "music", "hash": hash, "title": title, "files": files}
	if strings.HasPrefix(hash, "magnet:") {
		delete(payload, "hash")
		payload["magnet"] = hash
	}
	if len(torrentFile) > 0 {
		payload["torrent_file"] = torrentFile
	}
	if err := t.do(ctx, http.MethodPost, "/api/library/add", payload, &result); err != nil {
		return AddResult{}, err
	}
	return result, nil
}

func (t *Tiramisu) do(ctx context.Context, method, path string, payload, out interface{}) error {
	var body *bytes.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	} else {
		body = bytes.NewReader(nil)
	}
	request, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := t.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var apiError struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&apiError)
		return fmt.Errorf("library %s: status %d: %s", path, response.StatusCode, apiError.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// AudioRow is one projection row as the list reports it, reachability counters
// included when the request asked for failures.
type AudioRow struct {
	Path          string `json:"path"`
	SourcePath    string `json:"source_path"`
	ExternalID    string `json:"external_id"`
	Hash          string `json:"hash"`
	CueTrack      int    `json:"cue_track"`
	FailCount     int64  `json:"fail_count"`
	FirstFailNS   int64  `json:"first_fail_ns"`
	LastFailNS    int64  `json:"last_fail_ns"`
	ActiveSession bool   `json:"active_session"`
}

// AudioRows pages the whole music section with its reachability facts.
func (t *Tiramisu) AudioRows(ctx context.Context) ([]AudioRow, error) {
	var rows []AudioRow
	cursor := ""
	for {
		query := url.Values{"type": {"music"}, "limit": {"500"}, "failures": {"1"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Items      []AudioRow `json:"items"`
			NextCursor string     `json:"next_cursor"`
		}
		if err := t.do(ctx, http.MethodGet, "/api/library/list?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		rows = append(rows, page.Items...)
		if page.NextCursor == "" {
			return rows, nil
		}
		cursor = page.NextCursor
	}
}

// MediaServerType reads which player the box is configured for from the engine's
// config API: "plex" or "jellyfin". The panel is where the user sets it, so the
// controller follows it instead of asking again.
func (t *Tiramisu) MediaServerType(ctx context.Context) (string, error) {
	var config struct {
		MediaServerType string `json:"media_server_type"`
	}
	if err := t.do(ctx, http.MethodGet, "/api/config", nil, &config); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(config.MediaServerType)), nil
}

// IDStyleForPlayer maps the media server to the MusicBrainz id a projection should
// carry: Plex webhooks send the release track id, Jellyfin exposes the recording id
// as its MusicBrainzTrack provider.
func IDStyleForPlayer(serverType string) string {
	if strings.EqualFold(strings.TrimSpace(serverType), "jellyfin") {
		return "recording"
	}
	return "track"
}

// PrefixRemoveResult is the engine's answer to an album removal.
type PrefixRemoveResult struct {
	Removed           int  `json:"removed"`
	TorrentReferenced bool `json:"torrent_referenced"`
}

// RemovePrefix removes every projection under an album prefix.
func (t *Tiramisu) RemovePrefix(ctx context.Context, prefix string) (PrefixRemoveResult, error) {
	var result PrefixRemoveResult
	payload := map[string]string{"type": "music", "prefix": prefix}
	if err := t.do(ctx, http.MethodPost, "/api/library/remove", payload, &result); err != nil {
		return PrefixRemoveResult{}, err
	}
	return result, nil
}
