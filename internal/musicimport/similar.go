package musicimport

import (
	"context"
	"sort"
	"time"
)

// SimilarOptions tune the Deezer similarity pass.
type SimilarOptions struct {
	MinFans int           // an artist with fewer Deezer fans is too obscure to suggest; 0 = no floor
	MaxFans int           // an artist with more Deezer fans is too known to suggest; 0 = no limit
	Debut   time.Duration // the artist's first album or EP at most this old; 0 = any age
}

// similarSource is the slice of Deezer the similarity pass needs.
type similarSource interface {
	FindArtist(ctx context.Context, name string) (DeezerArtist, bool, error)
	RelatedArtists(ctx context.Context, id int) ([]DeezerArtist, error)
}

// deezerCandidates pools the Deezer related artists of the seeds: the seeds, the
// artists the libraries hold and the too known ones are dropped, the rest ranked by
// how many seeds point at them, then by the weight of those seeds. Each is resolved
// on MusicBrainz and offered with its latest studio album, when its debut is recent.
func (r *DiscoverRunner) deezerCandidates(ctx context.Context, seeds []Seed, index *LibraryIndex, now time.Time, logf func(string, ...any)) ([]candidate, error) {
	isSeed := map[string]bool{}
	for _, s := range seeds {
		isSeed[artistIdentity(s.Name)] = true
	}
	type pooled struct {
		artist DeezerArtist
		seeds  map[string]bool
		weight float64
	}
	pool := map[int]*pooled{}
	answered, known, obscure, owned := 0, 0, 0, 0
	var lastErr error
	for _, seed := range seeds {
		found, ok, err := r.Similar.FindArtist(ctx, seed.Name)
		if err == nil && ok {
			var related []DeezerArtist
			if related, err = r.Similar.RelatedArtists(ctx, found.ID); err == nil {
				answered++
				for _, a := range related {
					id := artistIdentity(a.Name)
					if id == "" || isSeed[id] {
						continue
					}
					if index.ArtistPresent("", a.Name) {
						owned++
						continue
					}
					if r.Options.Similar.MaxFans > 0 && a.Fans > r.Options.Similar.MaxFans {
						known++
						continue
					}
					if a.Fans < r.Options.Similar.MinFans {
						obscure++
						continue
					}
					p := pool[a.ID]
					if p == nil {
						p = &pooled{artist: a, seeds: map[string]bool{}}
						pool[a.ID] = p
					}
					if !p.seeds[seed.Name] {
						p.seeds[seed.Name] = true
						p.weight += seed.weight()
					}
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			lastErr = err
			logf("deezer seed %s: %v, skipped", seed.Name, err)
		} else {
			answered++
			logf("deezer seed %s: not found", seed.Name)
		}
	}
	if answered == 0 && lastErr != nil {
		return nil, lastErr
	}
	ranked := make([]*pooled, 0, len(pool))
	for _, p := range pool {
		ranked = append(ranked, p)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if len(ranked[i].seeds) != len(ranked[j].seeds) {
			return len(ranked[i].seeds) > len(ranked[j].seeds)
		}
		if ranked[i].weight != ranked[j].weight {
			return ranked[i].weight > ranked[j].weight
		}
		return ranked[i].artist.ID < ranked[j].artist.ID
	})

	limit := r.Options.MaxAlbums * triesPerAlbum
	var cutoff time.Time
	if r.Options.Similar.Debut > 0 {
		cutoff = now.Add(-r.Options.Similar.Debut)
	}
	var out []candidate
	unresolved, older := 0, 0
	for _, p := range ranked {
		if limit > 0 && len(out) >= limit {
			break
		}
		mbid, ok, err := r.Brainz.SearchArtist(ctx, p.artist.Name)
		if err != nil || !ok {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			unresolved++
			continue
		}
		groups, err := r.Brainz.ArtistAlbums(ctx, mbid, r.Options.AlbumTypes)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logf("similar artist %s: %v", p.artist.Name, err)
			continue
		}
		if !cutoff.IsZero() && !isNewArtist(groups, "", cutoff) {
			older++
			continue
		}
		latest, ok := latestStudioAlbum(groups, now)
		if !ok {
			continue
		}
		name := latest.Artist
		if name == "" {
			name = p.artist.Name
		}
		out = append(out, candidate{Artist: name, ArtistMBID: mbid, Title: latest.Title, RGID: latest.ID, rank: len(out)})
	}
	logf("similar artists (deezer): %d suggested, %d already yours, %d above %d fans, %d below %d fans, %d not on MusicBrainz, %d debuted too early, %d candidates",
		len(pool)+owned+known+obscure, owned, known, r.Options.Similar.MaxFans, obscure, r.Options.Similar.MinFans, unresolved, older, len(out))
	return out, nil
}
