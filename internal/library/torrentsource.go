package library

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// TorrentSource is what a release carries beyond its hash: the trackers its indexer
// announces it on and, when the indexer served a .torrent, the file itself. The
// file has the metadata, so the engine never waits for the swarm to send it, and its
// trackers may carry the user's own passkey.
type TorrentSource struct {
	Hash     string
	Trackers []string
	File     []byte
}

// ParseTorrentFile reads a .torrent: its info hash and its announce list.
func ParseTorrentFile(data []byte) (TorrentSource, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return TorrentSource{}, fmt.Errorf("parse .torrent: %w", err)
	}
	if _, err := mi.UnmarshalInfo(); err != nil {
		return TorrentSource{}, fmt.Errorf("parse .torrent info: %w", err)
	}
	var trackers []string
	for _, tier := range mi.UpvertedAnnounceList() {
		trackers = append(trackers, tier...)
	}
	return TorrentSource{Hash: mi.HashInfoBytes().HexString(), Trackers: MergeTrackers(trackers), File: data}, nil
}

// ParseMagnetSource reads a magnet: its info hash and its tr= trackers.
func ParseMagnetSource(magnet string) (TorrentSource, error) {
	hash := HashFromMagnet(magnet)
	if hash == "" {
		return TorrentSource{}, fmt.Errorf("magnet without an info hash")
	}
	var trackers []string
	if u, err := url.Parse(magnet); err == nil {
		trackers = u.Query()["tr"]
	}
	return TorrentSource{Hash: hash, Trackers: MergeTrackers(trackers)}, nil
}

// MergeTrackers joins tracker lists in order, dropping blanks and repeats.
func MergeTrackers(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, tr := range list {
			tr = strings.TrimSpace(tr)
			if tr == "" || seen[tr] {
				continue
			}
			seen[tr] = true
			out = append(out, tr)
		}
	}
	return out
}

// reSecret matches the per-user credentials trackers put in announce URLs, raw or
// URL-encoded inside a magnet.
var reSecret = regexp.MustCompile(`(?i)((?:pk|passkey|authkey|torrent_pass|uk|apikey)(?:=|%3D))[^&%\s"]+`)

// RedactSecrets masks tracker passkeys and API keys before a URL reaches a log: the
// log is shared when asking for help, the passkey is the user's account.
func RedactSecrets(s string) string {
	return reSecret.ReplaceAllString(s, "${1}***")
}

// releaseFile checks a request's .torrent against the hash the request names; an
// empty file is no file.
func releaseFile(data []byte, hash string) (TorrentSource, error) {
	if len(data) == 0 {
		return TorrentSource{}, nil
	}
	src, err := ParseTorrentFile(data)
	if err != nil {
		return TorrentSource{}, errf(http.StatusBadRequest, "torrent_file: %v", err)
	}
	if canonicalHashKey(src.Hash) != canonicalHashKey(hash) {
		return TorrentSource{}, errf(http.StatusBadRequest, "torrent_file is release %s, the request names %s", src.Hash, hash)
	}
	return src, nil
}

// torrentUploader is the engine side that takes a .torrent; *engines.GoStormClient
// has it.
type torrentUploader interface {
	UploadTorrent(ctx context.Context, data []byte, title string) (string, error)
}

// uploadReleaseFile hands the .torrent to the engine just before the add, which then
// finds the metadata and the file's trackers in place. A failed upload only costs the
// head start: the add proceeds from the magnet.
func (m *Manager) uploadReleaseFile(ctx context.Context, src TorrentSource, title string) {
	up, ok := m.cfg.GoStorm.(torrentUploader)
	if len(src.File) == 0 || !ok {
		return
	}
	if _, err := up.UploadTorrent(ctx, src.File, title); err != nil && m.cfg.Logger != nil {
		m.cfg.Logger.Printf("[LibraryAPI] WARNING: .torrent upload for %s failed, adding by magnet: %v", src.Hash, err)
	}
}
