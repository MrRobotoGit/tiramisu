package engines

import (
	"context"
	"strings"

	"tiramisu/internal/library"
	"tiramisu/internal/prowlarr"
)

// releaseMagnet prepares the release a sync picked: when it came from Prowlarr, its
// .torrent is fetched and handed to GoStorm (metadata at once, the indexer's trackers
// kept, a logged-in indexer's passkey included) and the magnet carries those trackers
// too. Any failure falls back to the magnet a hash alone gives, as before.
func releaseMagnet(ctx context.Context, pr *prowlarr.Client, gs *GoStormClient, hash, title, downloadURL string, logf func(string, ...any)) string {
	trackers := DefaultTrackers()
	if pr != nil && gs != nil && downloadURL != "" {
		src, err := pr.FetchTorrent(ctx, downloadURL)
		switch {
		case err != nil:
			logf("release file for %s: %v", title, err)
		case !strings.EqualFold(src.Hash, hash):
			logf("release file for %s: hash %s does not match %s, ignored", title, src.Hash, hash)
		default:
			if src.File != nil {
				if _, err := gs.UploadTorrent(ctx, src.File, title); err != nil {
					logf("release file for %s: upload: %v", title, err)
				}
			}
			trackers = library.MergeTrackers(trackers, src.Trackers)
		}
	}
	return BuildMagnet(hash, title, trackers)
}
