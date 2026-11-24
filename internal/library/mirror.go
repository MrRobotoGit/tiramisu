package library

import (
	"context"
	"errors"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// ErrStubConflict: the path holds another stub, or the torrent has no such file.
var ErrStubConflict = errors.New("library: the stub differs from the master's")

// MirrorEntry is one master stub to write here under the same relative path.
type MirrorEntry struct {
	Path      string // relative to the media root
	Hash      string
	Index     int
	Size      int64
	IMDB, Key string
	Score     int
	ShowIMDB  string
	HasInfo   bool // the torrent row is already registered here
}

// mirrorPath maps a master's relative path into the media dirs; kind is "movie" or "tv".
func (m *Manager) mirrorPath(rel, hash string) (abs, kind string, err error) {
	if strings.Contains(rel, "\\") || path.Clean(rel) != rel || !isStubName(rel) ||
		!reInfoHash.MatchString(hash) || len(hash) != 40 {
		return "", "", errf(http.StatusBadRequest, "invalid mirrored path %q", rel)
	}
	// The name is the master's and opaque: it may carry another hash8 and is never rewritten.
	switch {
	case strings.HasPrefix(rel, "movies/") && m.cfg.MoviesDir != "":
		abs, kind = filepath.Join(m.cfg.MoviesDir, filepath.FromSlash(strings.TrimPrefix(rel, "movies/"))), "movie"
	case strings.HasPrefix(rel, "tv/") && m.cfg.TVDir != "":
		abs, kind = filepath.Join(m.cfg.TVDir, filepath.FromSlash(strings.TrimPrefix(rel, "tv/"))), "tv"
	default:
		return "", "", errf(http.StatusBadRequest, "invalid mirrored path %q", rel)
	}
	if !m.insideMediaDirs(abs) {
		return "", "", errf(http.StatusBadRequest, "%s is outside the media directories", rel)
	}
	return abs, kind, nil
}

// Mirror writes a master's stub; it never replaces a stub of another hash or size.
func (m *Manager) Mirror(ctx context.Context, e MirrorEntry) error {
	abs, kind, err := m.mirrorPath(e.Path, e.Hash)
	if err != nil {
		return err
	}
	drop := false
	defer func() {
		if drop { // after the unlocks: the release takes the hash lock
			m.dropTorrentIfUnused(ctx, e.Hash)
		}
	}()
	defer m.lockHash(e.Hash)()
	if kind == "tv" && e.Key != "" {
		defer m.hashLocks.Lock("ep:" + e.Key)()
	}
	if fileExists(abs) {
		return m.mirrorExisting(kind, abs, e)
	}
	var files []FileStat
	if e.HasInfo {
		var ok bool
		if m.cfg.RowFiles == nil {
			return errf(http.StatusServiceUnavailable, "no stored torrent rows")
		}
		if files, ok = m.cfg.RowFiles(e.Hash); !ok {
			return errf(http.StatusNotFound, "torrent %s is not registered", e.Hash)
		}
	} else {
		if _, err := m.cfg.GoStorm.AddTorrent(ctx, BuildMagnet(e.Hash, mirrorTitle(abs), DefaultTrackers()), mirrorTitle(abs)); err != nil {
			drop = true
			return errf(http.StatusBadGateway, "Tentacle rejected the torrent: %v", err)
		}
		info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, e.Hash, defaultMetadataWait)
		if err != nil || info == nil {
			drop = true
			return errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", defaultMetadataWait, err)
		}
		files = info.FileStats
	}
	found := false
	for _, f := range files {
		found = found || (f.ID == e.Index && f.Length == e.Size)
	}
	if !found {
		drop = !e.HasInfo
		return ErrStubConflict
	}
	// Check and write under the path's lock: another stub may have landed during the wait.
	defer m.hashLocks.Lock("stub:" + abs)()
	if fileExists(abs) {
		err := m.mirrorExisting(kind, abs, e)
		drop = err != nil && !e.HasInfo
		return err
	}
	if err := WriteStub(abs, m.streamURL(e.Hash, e.Index), e.Size, BuildMagnet(e.Hash, mirrorTitle(abs), DefaultTrackers()), e.IMDB); err != nil {
		drop = !e.HasInfo
		return errf(http.StatusInternalServerError, "cannot write %s: %v", e.Path, err)
	}
	if err := m.registerMirrored(kind, abs, e); err != nil {
		return err
	}
	m.scheduleRefresh(m.section(kind))
	return nil
}

// mirrorExisting accepts the stub already at abs only if it is e's own.
func (m *Manager) mirrorExisting(kind, abs string, e MirrorEntry) error {
	st := readStub(abs)
	if st.Hash != e.Hash || st.FileIndex != e.Index || st.Size != e.Size {
		return ErrStubConflict
	}
	return m.registerMirrored(kind, abs, e)
}

// MirrorOnDisk reports whether the stub at e.Path is e's own: same hash, file index and size.
func (m *Manager) MirrorOnDisk(e MirrorEntry) bool {
	abs, _, err := m.mirrorPath(e.Path, e.Hash)
	if err != nil || !fileExists(abs) {
		return false
	}
	st := readStub(abs)
	return st.Hash == e.Hash && st.FileIndex == e.Index && st.Size == e.Size
}

// registerMirrored records an episode as the master registered it.
func (m *Manager) registerMirrored(kind, abs string, e MirrorEntry) error {
	if kind != "tv" || e.Key == "" || m.cfg.Registry == nil {
		return nil
	}
	cur, ok, err := m.cfg.Registry.GetEpisode(e.Key)
	if err != nil {
		return errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v", e.Key, err)
	}
	if ok && cur.Hash == e.Hash && cur.FilePath == abs && cur.QualityScore == e.Score && cur.ShowIMDB == e.ShowIMDB && cur.Source == "friends" {
		return nil
	}
	// The slave's own titles stay on acceptance: its episode keeps the key while its stub exists.
	if ok && cur.Source != "friends" && cur.FilePath != abs && fileExists(cur.FilePath) {
		m.cfg.Logger.Printf("[LibraryAPI] %s: the registry keeps this node's own %s", e.Key, cur.FilePath)
		return nil
	}
	if err := m.cfg.Registry.UpsertEpisode(e.Key, metadb.EpisodeEntry{EpisodeKey: e.Key, QualityScore: e.Score, Hash: e.Hash,
		FilePath: abs, Source: "friends", Created: time.Now().Unix(), ShowIMDB: e.ShowIMDB}); err != nil {
		return errf(http.StatusInternalServerError, "cannot register %s: %v", e.Key, err)
	}
	return nil
}

// mirrorTitle is the stub's name without its extension, for the magnet's display name.
func mirrorTitle(abs string) string {
	base := filepath.Base(abs)
	return base[:len(base)-len(filepath.Ext(base))]
}

// isStubName is friends.IsStubName: a non-empty name ending in .mkv, in any case (library does not import friends).
func isStubName(p string) bool {
	base := path.Base(p)
	return len(base) > len(".mkv") && strings.EqualFold(path.Ext(base), ".mkv")
}

// Unmirror removes a stub the master removed, through the consistent removal path, never blacklisting.
func (m *Manager) Unmirror(ctx context.Context, rel string) error {
	if strings.Contains(rel, "\\") || path.Clean(rel) != rel {
		return errf(http.StatusBadRequest, "invalid mirrored path %q", rel)
	}
	var abs string
	switch {
	case strings.HasPrefix(rel, "movies/") && m.cfg.MoviesDir != "":
		abs = filepath.Join(m.cfg.MoviesDir, filepath.FromSlash(strings.TrimPrefix(rel, "movies/")))
	case strings.HasPrefix(rel, "tv/") && m.cfg.TVDir != "":
		abs = filepath.Join(m.cfg.TVDir, filepath.FromSlash(strings.TrimPrefix(rel, "tv/")))
	default:
		return errf(http.StatusBadRequest, "invalid mirrored path %q", rel)
	}
	if !m.insideMediaDirs(abs) || !isStubName(filepath.ToSlash(abs)) {
		return errf(http.StatusBadRequest, "%s is outside the media directories", rel)
	}
	hash := readStub(abs).Hash
	if err := m.deleteStub(ctx, abs); err != nil {
		return errf(http.StatusInternalServerError, "cannot delete %s: %v", rel, err)
	}
	m.forgetEpisode(abs)
	if hash != "" {
		if _, err := m.ReleaseIfUnused(ctx, hash); err != nil {
			m.cfg.Logger.Printf("[LibraryAPI] WARNING: torrent %s not released: %v", hash, err)
		}
	}
	m.scheduleRefresh(m.section(m.kindOf(abs)))
	return nil
}
