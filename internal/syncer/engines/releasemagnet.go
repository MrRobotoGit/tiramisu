package engines

import (
	"context"
	"strings"

	"tiramisu/internal/library"
	"tiramisu/internal/prowlarr"
)

// fetchRelease fetches what a picked release's download link serves, once: it gives
// the hash of a release the indexer listed without one, and the .torrent and trackers
// releaseMagnetFrom hands on. ok is false only when no hash is known at the end; a
// failing link for a release that already had one falls back to the hash alone.
func fetchRelease(ctx context.Context, pr *prowlarr.Client, hash, title, downloadURL string, logf func(string, ...any)) (string, library.TorrentSource, bool) {
	hash = strings.ToLower(hash)
	if pr == nil || downloadURL == "" {
		return hash, library.TorrentSource{}, hash != ""
	}
	src, err := pr.FetchTorrent(ctx, downloadURL)
	switch {
	case err != nil:
		logf("release file for %s: %v", title, err)
		return hash, library.TorrentSource{}, hash != ""
	case hash == "":
		h := strings.ToLower(prowlarr.HexHash(src.Hash))
		if h == "" {
			logf("release file for %s: no usable info hash", title)
			return "", library.TorrentSource{}, false
		}
		return h, src, true
	case !strings.EqualFold(src.Hash, hash):
		logf("release file for %s: hash %s does not match %s, ignored", title, src.Hash, hash)
		return hash, library.TorrentSource{}, true
	}
	return hash, src, true
}

// releaseMagnetFrom hands the fetched .torrent to Tentacle (metadata at once, the
// indexer's trackers kept, a logged-in indexer's passkey included) and returns the
// magnet carrying those trackers too. An empty src gives the magnet a hash alone gives.
func releaseMagnetFrom(ctx context.Context, gs *GoStormClient, hash, title string, src library.TorrentSource, logf func(string, ...any)) string {
	trackers := DefaultTrackers()
	if gs != nil && src.File != nil {
		if _, err := gs.UploadTorrent(ctx, src.File, title); err != nil {
			logf("release file for %s: upload: %v", title, err)
		}
	}
	trackers = library.MergeTrackers(trackers, src.Trackers)
	return BuildMagnet(hash, title, trackers)
}
