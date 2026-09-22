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
}

// AddFile is one projection in an add request.
type AddFile struct {
	SourcePath          string `json:"source_path"`
	Path                string `json:"path"`
	ExternalID          string `json:"external_id,omitempty"`
	ExternalIDNamespace string `json:"external_id_ns,omitempty"`
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
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

// CommittedExternalIDs returns every committed projection's external identity, so a
// resumed run can skip albums the library already holds.
func (t *Tiramisu) CommittedExternalIDs(ctx context.Context) (map[string]bool, error) {
	present := make(map[string]bool)
	cursor := ""
	for {
		query := url.Values{"type": {"music"}, "limit": {"500"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Items []struct {
				ExternalID          string `json:"external_id"`
				ExternalIDNamespace string `json:"external_id_ns"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := t.do(ctx, http.MethodGet, "/api/library/list?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.ExternalID != "" {
				present[item.ExternalID] = true
			}
		}
		if page.NextCursor == "" {
			return present, nil
		}
		cursor = page.NextCursor
	}
}

// Inspect hydrates the torrent if needed and returns its file list.
func (t *Tiramisu) Inspect(ctx context.Context, hash, title string) ([]SourceFile, error) {
	var response struct {
		Files []SourceFile `json:"files"`
	}
	payload := map[string]string{"hash": hash, "title": title}
	if err := t.do(ctx, http.MethodPost, "/api/library/inspect", payload, &response); err != nil {
		return nil, err
	}
	return response.Files, nil
}

// Add files an album's projections.
func (t *Tiramisu) Add(ctx context.Context, hash, title string, files []AddFile) (AddResult, error) {
	var result AddResult
	payload := map[string]interface{}{"type": "music", "hash": hash, "title": title, "files": files}
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
	Hash          string `json:"hash"`
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
