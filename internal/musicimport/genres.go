package musicimport

import (
	"context"
	"sort"
	"time"
)

// GenreOptions turns on the genre pass: niche artists of the genres you listen to now,
// ranked by how close they sit to your artists, debut within Debut.
type GenreOptions struct {
	Enabled bool
	Count   int // how many of your genres are explored
	Debut   time.Duration
}

// genreSource is the slice of ListenBrainz the genre pass needs.
type genreSource interface {
	ArtistGenres(ctx context.Context, artistMBIDs []string) (map[string][]LBTag, error)
	TagRecordings(ctx context.Context, tag string, popBegin, popEnd, count int) ([]string, error)
	RecordingArtists(ctx context.Context, recordingMBIDs []string) (map[string]LBArtist, error)
}

const (
	// genreBatch keeps the metadata requests short enough for ListenBrainz to answer.
	genreBatch = 25
	// genreRecordings is how many recordings each genre contributes to the pool.
	genreRecordings = 200
	// genreShortlist is how many pooled artists are compared with your artists.
	genreShortlist = 150
)

// broadGenres steer nothing: every seed carries them.
var broadGenres = map[string]bool{"rock": true, "pop": true}

// genreCandidates pools the least played recordings of your main genres, keeps the
// artists you do not have, and ranks them: one of your recent artists among their
// neighbours counts most, then how many of your genres they span, then any artist you
// own among their neighbours. An artist with no link to yours is dropped.
func (r *DiscoverRunner) genreCandidates(ctx context.Context, index *LibraryIndex, now time.Time, logf func(string, ...any)) ([]candidate, error) {
	seeds := r.State.Seeds
	if len(seeds) == 0 {
		return nil, nil
	}

	// 1. Your genres, each seed's tags weighted by the seed.
	weight := map[string]float64{}
	name := map[string]string{}
	var ids []string
	for _, s := range seeds {
		if s.MBID != "" {
			ids = append(ids, s.MBID)
			weight[s.MBID] = s.weight()
			name[s.MBID] = s.Name
		}
	}
	scores := map[string]float64{}
	for start := 0; start < len(ids); start += genreBatch {
		tags, err := r.Tags.ArtistGenres(ctx, ids[start:min(start+genreBatch, len(ids))])
		if err != nil {
			return nil, err
		}
		for mbid, list := range tags {
			for _, t := range list {
				if !broadGenres[t.Tag] {
					scores[t.Tag] += weight[mbid]
				}
			}
		}
	}
	genres := make([]string, 0, len(scores))
	for g := range scores {
		genres = append(genres, g)
	}
	sort.Slice(genres, func(i, j int) bool {
		if scores[genres[i]] != scores[genres[j]] {
			return scores[genres[i]] > scores[genres[j]]
		}
		return genres[i] < genres[j]
	})
	if len(genres) > r.Options.Genres.Count {
		genres = genres[:r.Options.Genres.Count]
	}
	logf("genres: %v", genres)

	// 2. The niche end of each genre, resolved to artists you do not have.
	type pooled struct {
		artist LBArtist
		genres int
		score  float64
		seeds  []string
	}
	cutoff := debutCutoff(now, r.Options.Genres.Debut)
	pool := map[string]*pooled{}
	for _, g := range genres {
		recordings, err := r.Tags.TagRecordings(ctx, g, 60, 100, genreRecordings)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logf("genre %s: %v", g, err)
			continue
		}
		seen := map[string]bool{}
		for start := 0; start < len(recordings); start += genreBatch {
			artists, err := r.Tags.RecordingArtists(ctx, recordings[start:min(start+genreBatch, len(recordings))])
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				logf("genre %s artists: %v", g, err)
				continue
			}
			for _, a := range artists {
				if seen[a.MBID] || a.MBID == variousArtistsMBID || !mbidPattern.MatchString(a.MBID) || index.ArtistPresent(a.MBID, a.Name) {
					continue
				}
				if !cutoff.IsZero() && a.BeginYear > 0 && a.BeginYear < cutoff.Year() {
					continue
				}
				seen[a.MBID] = true
				p := pool[a.MBID]
				if p == nil {
					p = &pooled{artist: a}
					pool[a.MBID] = p
				}
				p.genres++
			}
		}
	}
	shortlist := make([]*pooled, 0, len(pool))
	for _, p := range pool {
		shortlist = append(shortlist, p)
	}
	sort.Slice(shortlist, func(i, j int) bool {
		if shortlist[i].genres != shortlist[j].genres {
			return shortlist[i].genres > shortlist[j].genres
		}
		return shortlist[i].artist.MBID < shortlist[j].artist.MBID
	})
	if len(shortlist) > genreShortlist {
		shortlist = shortlist[:genreShortlist]
	}

	// 3. Closeness to your artists, from the neighbours cache the new-artist pass shares.
	var ranked []*pooled
	for _, p := range shortlist {
		entry, ok := r.State.Neighbours[p.artist.MBID]
		if !ok || now.Sub(entry.Checked) > neighboursTTL {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fetched, err := r.neighbours(ctx, p.artist.MBID, now)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			entry = fetched
			r.State.Neighbours[p.artist.MBID] = entry
		}
		linked := false
		for i, id := range entry.IDs {
			if w, ok := weight[id]; ok {
				p.score += 2 * w
				p.seeds = append(p.seeds, name[id])
				linked = true
			} else if index.ArtistPresent(id, entry.Names[i]) {
				p.score += 0.5
				linked = true
			}
		}
		if linked {
			p.score += float64(p.genres)
			ranked = append(ranked, p)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].artist.MBID < ranked[j].artist.MBID
	})

	// 4. New artists only, with their latest studio album or EP already out.
	limit := r.Options.MaxAlbums * triesPerAlbum
	var out []candidate
	older := 0
	for _, p := range ranked {
		if limit > 0 && len(out) >= limit {
			break
		}
		groups, err := r.Brainz.ArtistAlbums(ctx, p.artist.MBID, r.Options.AlbumTypes)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logf("genre artist %s: %v", p.artist.Name, err)
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
		out = append(out, candidate{
			Artist: latest.Artist, ArtistMBID: p.artist.MBID, Title: latest.Title, RGID: latest.ID, rank: len(out),
		})
		if latest.Artist == "" {
			out[len(out)-1].Artist = p.artist.Name
		}
	}
	logf("genres: %d artists you do not have, %d close to yours, %d debuted before %s, %d new",
		len(pool), len(ranked), older, cutoffLabel(cutoff), len(out))
	return out, nil
}

// latestStudioAlbum is the newest album or EP already released, live and compilation
// groups aside.
func latestStudioAlbum(groups []ArtistReleaseGroup, now time.Time) (ArtistReleaseGroup, bool) {
	var best ArtistReleaseGroup
	var bestDate time.Time
	for _, g := range groups {
		if len(g.SecondaryTypes) > 0 {
			continue
		}
		date, ok := parsePartialDate(g.FirstReleaseDate)
		if !ok || date.After(now) {
			continue
		}
		if best.ID == "" || date.After(bestDate) {
			best, bestDate = g, date
		}
	}
	return best, best.ID != ""
}
