package library

import (
	"net/http"
	"time"

	"tiramisu/internal/metadb"
)

// AudioListRequest pages one section's projections. Cursor is the last path of
// the previous page.
type AudioListRequest struct {
	Type   string `json:"type"`
	Prefix string `json:"prefix"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	// WithFailures adds the reachability counters of each row's torrent, the facts a
	// reaper applies its threshold to. Off by default: it costs one extra query.
	WithFailures bool
}

// AudioListItem is reconciliation data, not media metadata: enough for a
// controller to diff what it sent against what the engine holds.
type AudioListItem struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	SourcePath string `json:"source_path"`
	FileIndex  int    `json:"file_index"`
	Size       int64  `json:"size"`
	MtimeNS    int64  `json:"mtime_ns"`
	State      string `json:"state"`

	ExternalID          string `json:"external_id"`
	ExternalIDNamespace string `json:"external_id_ns"`
	// CueTrack is the image track a projection serves; absent for a whole file.
	CueTrack int `json:"cue_track,omitempty"`

	// Reachability facts, present only when the request asked for failures.
	FailCount   int64 `json:"fail_count,omitempty"`
	FirstFailNS int64 `json:"first_fail_ns,omitempty"`
	LastFailNS  int64 `json:"last_fail_ns,omitempty"`
	// ActiveSession marks a row whose torrent is being played right now. An album in
	// this state must not be reaped: its counter is acquitted when the session closes.
	ActiveSession bool `json:"active_session,omitempty"`
}

type AudioListResponse struct {
	Items      []AudioListItem `json:"items"`
	NextCursor string          `json:"next_cursor"`
}

// audioProjectionPager is the paging half of the registry, reached by assertion
// as manager.go already does for ClearMetadataFailure.
type audioProjectionPager interface {
	AudioProjectionPage(section, pathPrefix, afterPath string, limit int) ([]metadb.AudioProjection, error)
}

// audioFailureReader is the reachability half a registry may also implement. Kept
// apart from the pager so a registry that cannot answer failures still pages.
type audioFailureReader interface {
	MetadataFailuresFor(hashes []string) (map[string]metadb.FailureStat, error)
}

// ListAudio returns one page of committed projections, asking for one row beyond
// the page so it can report a cursor without counting the library.
func (m *Manager) ListAudio(req AudioListRequest) (*AudioListResponse, error) {
	section, canonical := SectionForType(req.Type)
	if !canonical || !IsAudioSection(section) {
		return nil, errf(http.StatusBadRequest, "unknown type %q: use music or audiobook", req.Type)
	}
	pager, ok := m.cfg.AudioProjections.(audioProjectionPager)
	if !ok {
		return nil, errf(http.StatusServiceUnavailable, "the audio projection registry is unavailable")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = metadb.DefaultAudioProjectionPageLimit
	} else if limit > metadb.MaxAudioProjectionPageLimit {
		limit = metadb.MaxAudioProjectionPageLimit
	}
	rows, err := pager.AudioProjectionPage(string(section), req.Prefix, req.Cursor, limit+1)
	if err != nil {
		return nil, audioErr(err)
	}

	next := ""
	switch {
	case len(rows) > limit:
		rows = rows[:limit]
		next = rows[len(rows)-1].VirtualPath
	case len(rows) == limit && limit >= metadb.MaxAudioProjectionPageLimit:
		// At the cap the probe row is clamped away by the pager itself, so a full
		// page cannot be told from a last page without asking for one more.
		more, err := pager.AudioProjectionPage(string(section), req.Prefix, rows[len(rows)-1].VirtualPath, 1)
		if err != nil {
			return nil, audioErr(err)
		}
		if len(more) > 0 {
			next = rows[len(rows)-1].VirtualPath
		}
	}
	failures := map[string]metadb.FailureStat{}
	if req.WithFailures {
		if reader, ok := m.cfg.AudioProjections.(audioFailureReader); ok {
			hashes := make([]string, 0, len(rows))
			seen := make(map[string]bool, len(rows))
			for _, row := range rows {
				if row.Hash != "" && !seen[row.Hash] {
					seen[row.Hash] = true
					hashes = append(hashes, row.Hash)
				}
			}
			stats, err := reader.MetadataFailuresFor(hashes)
			if err != nil {
				return nil, audioErr(err)
			}
			failures = stats
		}
	}
	items := make([]AudioListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, AudioListItem{
			Type:       req.Type,
			Path:       row.VirtualPath,
			Hash:       row.Hash,
			SourcePath: row.SourcePath,
			FileIndex:  row.FileIndex,
			Size:       row.Size,
			MtimeNS:    row.MtimeNS,
			State:      string(row.State),

			ExternalID:          row.ExternalID,
			ExternalIDNamespace: row.ExternalIDNamespace,
			CueTrack:            row.CueTrack,
			FailCount:           failures[row.Hash].FailCount,
			FirstFailNS:         failures[row.Hash].FirstFail * int64(time.Second),
			LastFailNS:          failures[row.Hash].LastFail * int64(time.Second),
			ActiveSession:       m.cfg.ActiveSession != nil && m.cfg.ActiveSession(row.Hash),
		})
	}
	return &AudioListResponse{Items: items, NextCursor: next}, nil
}
