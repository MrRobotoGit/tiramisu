package musicimport

import (
	"context"
	"fmt"
	"strings"

	"tiramisu/internal/prowlarr"
)

// AlbumIndexer is the indexer side of a manual album search; *prowlarr.Client is one.
type AlbumIndexer interface {
	SearchWithOptions(ctx context.Context, query string, opts prowlarr.SearchOptions) ([]prowlarr.ProwlarrResult, error)
	ResolveHash(downloadURL string) string
}

// AlbumBrainz is the MusicBrainz side of filing one album; *MusicBrainz is one.
type AlbumBrainz interface {
	GroupReleases(ctx context.Context, rgID string) ([]RecordingRelease, error)
	ReleaseDetails(ctx context.Context, releaseID string) (ReleaseGroup, []ReleaseTrack, bool, error)
}

// SearchAlbumCandidates runs the sync's two queries and returns up to limit lossless
// candidates, best first, each with its hash resolved. The sync stops at the first.
func SearchAlbumCandidates(ctx context.Context, indexer AlbumIndexer, indexerIDs []int, artist, title string, minSeeders int, maxSizeBytes int64, limit int, logf func(string, ...any)) []Candidate {
	artist, title = asciiPunctuation(artist), asciiPunctuation(title)
	var out []Candidate
	seen := map[string]bool{}
	for _, query := range []string{
		strings.TrimSpace(artist + " " + title + " FLAC"),
		strings.TrimSpace(artist + " " + title),
	} {
		if len(out) >= limit || ctx.Err() != nil {
			break
		}
		results, err := indexer.SearchWithOptions(ctx, query, prowlarr.SearchOptions{IndexerIDs: indexerIDs})
		if err != nil {
			logf("prowlarr %q: %v", query, err)
			continue
		}
		for _, c := range SelectCandidates(results, artist, title, minSeeders, maxSizeBytes, limit) {
			if len(out) >= limit {
				break
			}
			if c.Hash == "" {
				if c.Hash = strings.ToLower(indexer.ResolveHash(c.DownloadURL)); c.Hash == "" {
					logf("cannot resolve a hash for %q", c.Title)
					continue
				}
			}
			c.Hash = strings.ToLower(c.Hash)
			if seen[c.Hash] {
				continue
			}
			seen[c.Hash] = true
			out = append(out, c)
		}
	}
	return out
}

// AddAlbum files a chosen candidate the way the sync does: the release group's best
// edition gives the tracklist, the indexer's file comes along when it matches.
func AddAlbum(ctx context.Context, library LibraryWriter, brainz AlbumBrainz, indexer AlbumIndexer, artist, title, rgID string, c Candidate, idStyle string, logf func(string, ...any)) (AddResult, error) {
	var tracks []ReleaseTrack
	releases, err := brainz.GroupReleases(ctx, rgID)
	if err != nil {
		logf("releases of %s: %v", rgID, err)
	}
	var best *RecordingRelease
	for i := range releases {
		if best == nil || betterRelease(releases[i], *best) {
			best = &releases[i]
		}
	}
	if best != nil {
		if _, t, ok, err := brainz.ReleaseDetails(ctx, best.ReleaseID); err != nil {
			logf("tracklist %s: %v", best.ReleaseID, err)
		} else if ok {
			tracks = t
		}
	}
	if indexer != nil {
		c = withRelease(ctx, indexer, c, logf)
	}
	group := ReleaseGroup{ID: rgID, Artist: artist, Title: title}
	result, err := applyFiles(ctx, library, artist, title, group, tracks, c, idStyle)
	if err != nil {
		return AddResult{}, fmt.Errorf("%s / %s: %w", artist, title, err)
	}
	return result, nil
}
