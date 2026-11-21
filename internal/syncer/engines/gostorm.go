package engines

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tiramisu/internal/library"
)

// Backend is the engine side of GoStormClient: the in-process engine in production
// (EngineBackend); tests fake the old JSON API with enginestest.HTTP.
type Backend interface {
	Add(ctx context.Context, link, title string) (hash string, confirmed bool, err error)
	AddTorrentFile(ctx context.Context, data []byte, title string) (string, error)
	// Info waits until the torrent's files are known or ctx ends.
	Info(ctx context.Context, hash string) (*TorrentStats, error)
	Remove(ctx context.Context, hash string) error
	Refs(ctx context.Context) ([]TorrentRef, error)
	Hydrated(ctx context.Context) ([]string, error)
}

// GoStormClient is the torrent API of the syncs and the library.
type GoStormClient struct {
	b       Backend
	baseURL string // stub URL prefix only (gostorm_url): nothing listens there (design V8)
}

// NewGoStormClient wraps b; stubBase prefixes the stub URLs the syncs write.
func NewGoStormClient(b Backend, stubBase string) *GoStormClient {
	return &GoStormClient{b: b, baseURL: strings.TrimRight(stubBase, "/")}
}

// TorrentStats and FileStat live in internal/library: the same shapes are returned by
// the /api/library endpoints, and aliasing them lets *GoStormClient satisfy
// library.GoStorm with no adapter.
type TorrentStats = library.TorrentStats

// TorrentRef is a torrent's hash and title, from the engine's light listing.
type TorrentRef = library.TorrentRef

type FileStat = library.FileStat

var magnetHash = regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)

// AddTorrent adds a magnet and returns its hash. Callers that must not mistake an
// unacknowledged add for a statement about the swarm use AddTorrentConfirmed.
func (c *GoStormClient) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	hash, _, err := c.AddTorrentConfirmed(ctx, magnet, title)
	return hash, err
}

// AddTorrentConfirmed reports confirmed=false when the engine took the add without naming
// the hash: the torrent may never have been taken, so its silence says nothing.
func (c *GoStormClient) AddTorrentConfirmed(ctx context.Context, magnet, title string) (string, bool, error) {
	match := magnetHash.FindStringSubmatch(magnet)
	if len(match) < 2 {
		return "", false, fmt.Errorf("cannot extract hash from magnet")
	}
	hash, confirmed, err := c.b.Add(ctx, magnet, title)
	if err != nil {
		return "", false, err
	}
	if hash == "" {
		return strings.ToLower(match[1]), false, nil
	}
	return strings.ToLower(hash), confirmed, nil
}

// UploadTorrent hands the engine a release's .torrent (saved to its DB): the metadata is
// known at once and the file's own trackers are kept for every later wake.
func (c *GoStormClient) UploadTorrent(ctx context.Context, data []byte, title string) (string, error) {
	hash, err := c.b.AddTorrentFile(ctx, data, title)
	if err != nil {
		return "", err
	}
	return strings.ToLower(hash), nil
}

// GetTorrentInfo waits up to maxWait seconds for the torrent's files.
func (c *GoStormClient) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	wctx, cancel := context.WithTimeout(ctx, time.Duration(maxWait)*time.Second)
	defer cancel()
	info, err := c.b.Info(wctx, hash)
	if err == nil && len(info.FileStats) > 0 {
		return info, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	short := hash
	if len(short) > 8 {
		short = short[:8]
	}
	return nil, fmt.Errorf("metadata timeout for %s… (waited %ds)", short, maxWait)
}

// RemoveTorrent removes a torrent and its DB row.
func (c *GoStormClient) RemoveTorrent(ctx context.Context, hash string) error {
	return c.b.Remove(ctx, hash)
}

// ListTorrentRefs returns the hash and title of every torrent the engine holds, stored or
// active, without building any torrent's status.
func (c *GoStormClient) ListTorrentRefs(ctx context.Context) ([]TorrentRef, error) {
	return c.b.Refs(ctx)
}

// ActiveHashes returns the torrents hydrated in memory: a sync re-using a stored torrent
// has it here before its stub exists.
func (c *GoStormClient) ActiveHashes(ctx context.Context) ([]string, error) {
	hs, err := c.b.Hydrated(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, strings.ToLower(h))
	}
	return out, nil
}

// TitleFromFilename extracts a clean display title from an MKV filename.
// e.g. "Your_Friends_Neighbors_S01E09_17361ba1.mkv" → "Your Friends Neighbors S01E09"
func TitleFromFilename(filename string) string {
	s := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Remove trailing _hash8 (8 hex chars)
	if re := regexp.MustCompile(`(?i)_[a-f0-9]{8}$`); re.MatchString(s) {
		s = s[:len(s)-9]
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	return strings.TrimSpace(s)
}

// BuildMagnet and DefaultTrackers live in internal/library, shared with the
// /api/library endpoints.
func BuildMagnet(infoHash, name string, trackers []string) string {
	return library.BuildMagnet(infoHash, name, trackers)
}

func DefaultTrackers() []string { return library.DefaultTrackers() }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
