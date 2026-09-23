package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// AudioRemovalRegistry is the removal half of the projection registry: it claims a
// committed projection for removal, forgets it once its file is gone, and answers how
// many rows still reference a torrent. It is kept separate from AudioProjectionRegistry
// so readers and adders are not forced to implement what they never call.
type AudioRemovalRegistry interface {
	MarkAudioProjectionRemoving(section, virtualPath string, updatedAtNS int64) (bool, error)
	DeleteAudioProjection(section, virtualPath string) error
	AudioProjectionsByHash(hash string) ([]metadb.AudioProjection, error)
	// Prefix removal reads the album and claims exactly the rows it read. Matching on
	// path components keeps "Artist/Album" away from "Artist/Album2"; returning rows
	// already in removing lets a later call resume an interrupted album.
	AudioProjectionsByPrefix(section, prefix string) ([]metadb.AudioProjection, error)
	MarkAudioProjectionsRemovingPaths(section string, virtualPaths []string, updatedAtNS int64) (int, error)
}

// AudioPrefixRemoveResponse reports album-granular removal: how many projections the
// call finished (claimed now or resumed from an interrupted removal), and whether
// their torrent is still referenced by another album.
type AudioPrefixRemoveResponse struct {
	Type              string `json:"type"`
	Prefix            string `json:"prefix"`
	Removed           int    `json:"removed"`
	TorrentReferenced bool   `json:"torrent_referenced"`
}

// AudioRemoveResponse reports an exact-path audio removal. Removed is false with State
// "absent" when the projection was already gone, which makes retries harmless.
type AudioRemoveResponse struct {
	Type              string `json:"type"`
	Path              string `json:"path"`
	Removed           bool   `json:"removed"`
	State             string `json:"state,omitempty"`
	TorrentReferenced bool   `json:"torrent_referenced"`
}

// RemoveAudio removes exactly one audio projection by its section-relative path. Phase 1
// removal is exact-path only: no hash, prefix or recursive forms, and no blacklist,
// which is not audio lifecycle state. The torrent itself is never dropped here: while
// any other row references it the fail-closed guard keeps it alive, and once none does
// it expires through the engine's own idle policy.
func (m *Manager) RemoveAudio(ctx context.Context, req RemoveRequest) (*AudioRemoveResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	// Routing is decided from the type alone, before validation, so every video
	// request reaches the legacy path unchanged.
	section, canonical := SectionForType(req.Type)
	if !canonical || !IsAudioSection(section) {
		return nil, &Error{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("type %q is not handled by RemoveAudio", req.Type),
			Err:     ErrRequestNotAudio,
		}
	}
	if req.Blacklist {
		return nil, errf(http.StatusBadRequest, "blacklist is not applicable to audio removal")
	}
	virtualPath := strings.TrimSpace(req.Path)
	if virtualPath == "" {
		return nil, errf(http.StatusBadRequest, "path is required for audio removal")
	}
	if err := checkRel(virtualPath); err != nil {
		return nil, errf(http.StatusBadRequest, "invalid audio path: %v", err)
	}
	if m.cfg.AudioProjections == nil {
		return nil, errf(http.StatusServiceUnavailable, "the audio projection registry is unavailable")
	}
	if m.cfg.AudioRemoval == nil {
		return nil, errf(http.StatusServiceUnavailable, "the audio projection registry does not support removal")
	}
	if m.cfg.AudioRoot == "" {
		return nil, errf(http.StatusServiceUnavailable, "no audio root configured")
	}

	absent := &AudioRemoveResponse{Type: req.Type, Path: virtualPath, Removed: false, State: "absent"}
	row, found, err := m.cfg.AudioProjections.GetAudioProjection(string(section), virtualPath)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "look up audio projection: %v", err)
	}
	if !found {
		return absent, nil
	}
	switch row.State {
	case metadb.AudioCommitted:
		claimed, err := m.cfg.AudioRemoval.MarkAudioProjectionRemoving(string(section), virtualPath, time.Now().UnixNano())
		if err != nil {
			return nil, errf(http.StatusInternalServerError, "claim audio projection for removal: %v", err)
		}
		if !claimed {
			// Lost a race with another removal: the row is no longer committed.
			return absent, nil
		}
	case metadb.AudioRemoving:
		// An interrupted removal: finish it so the retry is idempotent.
	default:
		// An add owns the path right now; its outcome decides what the caller sees.
		return absent, nil
	}

	// Fail closed before touching the file: the namespace and the caches stop answering
	// for the path while its stub is still on disk, so nothing new opens what is about
	// to disappear. A reader that already resolved the path keeps its handle.
	if m.cfg.UnpublishAudioPath != nil {
		m.cfg.UnpublishAudioPath(AudioProjection{
			Section:             Section(row.Section),
			VirtualPath:         row.VirtualPath,
			Hash:                row.Hash,
			FileIndex:           row.FileIndex,
			Size:                row.Size,
			MtimeNS:             row.MtimeNS,
			UpdatedAtNS:         row.UpdatedAtNS,
			ExternalID:          row.ExternalID,
			ExternalIDNamespace: row.ExternalIDNamespace,
		})
	}

	writer, err := OpenSectionWriter(filepath.Join(m.cfg.AudioRoot, string(section)))
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "open audio section: %v", err)
	}
	defer writer.Close()
	if id, err := writer.Identity(row.VirtualPath); err == nil {
		removed, err := writer.RemoveStagedIfIdentity(row.VirtualPath, id)
		if err != nil {
			return nil, errf(http.StatusInternalServerError, "remove audio stub: %v", err)
		}
		if !removed {
			if _, statErr := writer.Identity(row.VirtualPath); statErr == nil {
				// Another writer replaced the file between the two calls: the
				// projection's own bytes are gone, but the file now holding the name
				// is not ours to delete. The row goes, so a retry cannot mistake the
				// replacement for the projection, and the caller is told the path is
				// still occupied.
				if err := m.cfg.AudioRemoval.DeleteAudioProjection(string(section), virtualPath); err != nil {
					return nil, errf(http.StatusInternalServerError, "forget audio projection: %v", err)
				}
				return nil, errf(http.StatusConflict, "the file at %s is not this projection's stub and was left in place", virtualPath)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errf(http.StatusInternalServerError, "stat audio stub: %v", err)
	}
	if err := writer.PruneEmptyDirs(row.VirtualPath); err != nil {
		return nil, errf(http.StatusInternalServerError, "prune audio directories: %v", err)
	}
	if err := m.cfg.AudioRemoval.DeleteAudioProjection(string(section), virtualPath); err != nil {
		return nil, errf(http.StatusInternalServerError, "forget audio projection: %v", err)
	}

	m.scheduleRefresh(m.audioSection(section))
	rows, err := m.cfg.AudioRemoval.AudioProjectionsByHash(row.Hash)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "count audio torrent references: %v", err)
	}
	return &AudioRemoveResponse{
		Type:              req.Type,
		Path:              virtualPath,
		Removed:           true,
		TorrentReferenced: len(rows) > 0,
	}, nil
}

// RemoveAudioPrefix removes every projection under one album prefix, in the same
// order a single removal uses per row: claim, unpublish, unlink by identity, prune,
// forget. The prefix must resolve to one torrent: an album is one torrent, and a
// prefix that spans several is a directory, not an album.
func (m *Manager) RemoveAudioPrefix(ctx context.Context, req RemoveRequest) (*AudioPrefixRemoveResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	section, canonical := SectionForType(req.Type)
	if !canonical || !IsAudioSection(section) {
		return nil, &Error{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("type %q is not handled by RemoveAudioPrefix", req.Type),
			Err:     ErrRequestNotAudio,
		}
	}
	if req.Blacklist {
		return nil, errf(http.StatusBadRequest, "blacklist is not applicable to audio removal")
	}
	prefix := strings.TrimSpace(req.Prefix)
	if prefix == "" {
		return nil, errf(http.StatusBadRequest, "prefix is required for prefix removal")
	}
	if err := checkRel(prefix); err != nil {
		return nil, errf(http.StatusBadRequest, "invalid audio prefix: %v", err)
	}
	if m.cfg.AudioRemoval == nil {
		return nil, errf(http.StatusServiceUnavailable, "the audio projection registry does not support removal")
	}
	if m.cfg.AudioRoot == "" {
		return nil, errf(http.StatusServiceUnavailable, "no audio root configured")
	}

	rows, err := m.cfg.AudioRemoval.AudioProjectionsByPrefix(string(section), prefix)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "list audio projections under %s: %v", prefix, err)
	}
	if len(rows) == 0 {
		return &AudioPrefixRemoveResponse{Type: req.Type, Prefix: prefix, Removed: 0}, nil
	}
	hash := rows[0].Hash
	hashes := map[string]bool{}
	for _, row := range rows {
		hashes[row.Hash] = true
	}
	if len(hashes) > 1 {
		// Name the torrents: the caller can only act on the conflict if it knows which
		// ones are involved, and Phase 1 has no per-hash removal to point at.
		listed := make([]string, 0, len(hashes))
		for h := range hashes {
			if len(h) > 12 {
				h = h[:12]
			}
			listed = append(listed, h)
		}
		sort.Strings(listed)
		return nil, errf(http.StatusConflict,
			"%s holds projections from %d torrents (%s); prefix removal needs exactly one",
			prefix, len(hashes), strings.Join(listed, ", "))
	}
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, row.VirtualPath)
	}
	if _, err := m.cfg.AudioRemoval.MarkAudioProjectionsRemovingPaths(string(section), paths, time.Now().UnixNano()); err != nil {
		return nil, errf(http.StatusInternalServerError, "claim audio projections under %s: %v", prefix, err)
	}

	writer, err := OpenSectionWriter(filepath.Join(m.cfg.AudioRoot, string(section)))
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "open audio section: %v", err)
	}
	defer writer.Close()
	for _, row := range rows {
		if m.cfg.UnpublishAudioPath != nil {
			m.cfg.UnpublishAudioPath(AudioProjection{
				Section:             Section(row.Section),
				VirtualPath:         row.VirtualPath,
				Hash:                row.Hash,
				FileIndex:           row.FileIndex,
				Size:                row.Size,
				MtimeNS:             row.MtimeNS,
				UpdatedAtNS:         row.UpdatedAtNS,
				ExternalID:          row.ExternalID,
				ExternalIDNamespace: row.ExternalIDNamespace,
			})
		}
		if id, err := writer.Identity(row.VirtualPath); err == nil {
			if _, err := writer.RemoveStagedIfIdentity(row.VirtualPath, id); err != nil {
				return nil, errf(http.StatusInternalServerError, "remove %s: %v", row.VirtualPath, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errf(http.StatusInternalServerError, "stat %s: %v", row.VirtualPath, err)
		}
		if err := writer.PruneEmptyDirs(row.VirtualPath); err != nil {
			return nil, errf(http.StatusInternalServerError, "prune %s: %v", row.VirtualPath, err)
		}
		if err := m.cfg.AudioRemoval.DeleteAudioProjection(row.Section, row.VirtualPath); err != nil {
			return nil, errf(http.StatusInternalServerError, "forget %s: %v", row.VirtualPath, err)
		}
	}

	m.scheduleRefresh(m.audioSection(section))
	remaining, err := m.cfg.AudioRemoval.AudioProjectionsByHash(hash)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "count audio torrent references: %v", err)
	}
	return &AudioPrefixRemoveResponse{
		Type:              req.Type,
		Prefix:            prefix,
		Removed:           len(rows),
		TorrentReferenced: len(remaining) > 0,
	}, nil
}
