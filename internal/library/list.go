package library

import (
	"net/http"

	"tiramisu/internal/metadb"
)

// AudioListRequest pages one section's projections. Cursor is the last path of
// the previous page.
type AudioListRequest struct {
	Type   string `json:"type"`
	Prefix string `json:"prefix"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
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
		})
	}
	return &AudioListResponse{Items: items, NextCursor: next}, nil
}
