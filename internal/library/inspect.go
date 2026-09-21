package library

import (
	"context"
	"net/http"
	"strings"
)

// InspectRequest identifies a torrent to read the file list of. It selects
// nothing: naming and file choice stay with the caller.
type InspectRequest struct {
	Hash         string `json:"hash"`
	Magnet       string `json:"magnet"`
	Title        string `json:"title"`
	MetadataWait int    `json:"metadata_wait"`
}

// InspectFile is one source file as the engine sees it. source_path is what a
// caller selects by; file_index is GoStorm's own resolved state.
type InspectFile struct {
	SourcePath string `json:"source_path"`
	FileIndex  int    `json:"file_index"`
	Size       int64  `json:"size"`
}

type InspectResponse struct {
	Hash  string        `json:"hash"`
	Files []InspectFile `json:"files"`
}

// Inspect reports a torrent's files. Metadata that never arrives is an error, not
// an empty list: "not ready" and "no files" are different answers.
func (m *Manager) Inspect(ctx context.Context, req InspectRequest) (*InspectResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, errf(http.StatusBadRequest, "title is required")
	}
	// Identity resolution mirrors validate(): a magnet's own info hash wins, so
	// cleanup later removes the torrent the engine was actually asked to add.
	hash := strings.ToLower(strings.TrimSpace(req.Hash))
	magnet := strings.TrimSpace(req.Magnet)
	if magnet != "" {
		if fromMagnet := HashFromMagnet(magnet); fromMagnet != "" {
			hash = fromMagnet
		} else if hash == "" {
			return nil, errf(http.StatusBadRequest, "magnet carries no info hash")
		}
	}
	if hash == "" {
		return nil, errf(http.StatusBadRequest, "hash or magnet is required")
	}
	if !reInfoHash.MatchString(hash) {
		return nil, errf(http.StatusBadRequest, "malformed info hash %q", hash)
	}
	if magnet == "" {
		magnet = BuildMagnet(hash, title, DefaultTrackers())
	}

	// Held across ownership, add and cleanup, keyed on the canonical spelling so a
	// base32 magnet and its hex form are one torrent.
	lockKey := canonicalHashKey(hash)
	defer m.lockHash(lockKey)()

	known, ok := m.knownTorrentHashes(ctx)
	if !ok {
		// Adding without knowing what was already there would leak on failure:
		// there would be no way to tell whether this call hydrated the torrent.
		return nil, errf(http.StatusBadGateway, "cannot list torrents to establish ownership")
	}
	preexisting := known[lockKey]

	addedHash, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, title)
	if err != nil || addedHash == "" {
		return nil, errf(http.StatusBadGateway, "gostorm rejected the torrent: %v", err)
	}
	engineHash := strings.ToLower(strings.TrimSpace(addedHash))
	if !reInfoHash.MatchString(engineHash) {
		// Hydrated under the hash we asked for, so that is the one to drop.
		if !preexisting {
			m.dropTorrent(ctx, hash)
		}
		return nil, errf(http.StatusBadGateway, "gostorm returned a malformed info hash %q", addedHash)
	}
	// A base32 magnet comes back in hex, and everything from here is keyed on the
	// spelling the engine reported, so the lock has to cover it too.
	if engineKey := canonicalHashKey(engineHash); engineKey != lockKey {
		defer m.lockHash(engineKey)()
		preexisting = preexisting || known[engineKey]
	}

	wait := req.MetadataWait
	if wait <= 0 {
		wait = defaultMetadataWait
	} else if wait > maxMetadataWait {
		wait = maxMetadataWait
	}
	info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, engineHash, wait)
	if err != nil || info == nil {
		if !preexisting {
			m.dropTorrent(ctx, engineHash)
		}
		return nil, errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", wait, err)
	}

	// Non-nil even when empty: an empty array and a null are different answers.
	files := make([]InspectFile, 0, len(info.FileStats))
	for _, file := range info.FileStats {
		files = append(files, InspectFile{SourcePath: file.Path, FileIndex: file.ID, Size: file.Length})
	}
	return &InspectResponse{Hash: engineHash, Files: files}, nil
}

// knownTorrentHashes snapshots what the engine already holds. A failed listing
// reports not-ok: the caller must not add without it.
func (m *Manager) knownTorrentHashes(ctx context.Context) (map[string]bool, bool) {
	torrents, err := m.cfg.GoStorm.ListTorrents(ctx)
	if err != nil {
		// Logged because this fails the whole request: without it the caller sees
		// only "cannot establish ownership" and the cause is invisible.
		m.cfg.Logger.Printf("[LibraryAPI] WARNING: cannot list torrents: %v", err)
		return nil, false
	}
	known := make(map[string]bool, len(torrents))
	for _, torrent := range torrents {
		known[canonicalHashKey(torrent.Hash)] = true
	}
	return known, true
}
