package musicimport

import (
	"context"
	"sort"
	"time"
)

// NewArtistOptions turns on the new-artist pass: albums and EPs released in the last
// Window by artists you do not have, whose nearest artists on ListenBrainz you do,
// and whose first album or EP is no older than Debut.
type NewArtistOptions struct {
	Enabled bool
	Window  time.Duration // the feed serves at most 90 days
	Debut   time.Duration
}

// artistNeighbours is the cached answer for one new artist; no ids means ListenBrainz
// has no similarity data for it yet.
type artistNeighbours struct {
	IDs     []string  `json:"ids,omitempty"`
	Names   []string  `json:"names,omitempty"`
	Checked time.Time `json:"checked"`
}

const (
	// neighboursTTL is how long a cached answer is trusted: similarity data grows
	// with the listens, so an unknown artist is asked again after a month.
	neighboursTTL = 30 * 24 * time.Hour
	// neighboursCheckpoint is how many lookups pass between two state flushes, so a
	// stopped first scan keeps what it paid for.
	neighboursCheckpoint = 200
)

// nearestArtists asks for the closest ten artists (easy = similarity ranks 0-20).
var nearestArtists = RadioOptions{Mode: "easy", MaxSimilarArtists: 10, MaxRecordingsPerArtist: 1, PopBegin: 0, PopEnd: 100}

// newArtistCandidates filters the sitewide fresh releases down to new artists that
// sound like yours, strongest match first: an artist whose neighbours include more of
// your artists ranks higher, then the one closer to the artists you play most.
func (r *DiscoverRunner) newArtistCandidates(ctx context.Context, index *LibraryIndex, now time.Time, logf func(string, ...any)) ([]candidate, error) {
	days := int(r.Options.NewArtists.Window.Hours() / 24)
	days = max(1, min(days, 90))
	releases, err := r.Listen.FreshReleases(ctx, days)
	if err != nil {
		return nil, err
	}

	// 1. One release per artist not in the libraries, the most recent.
	allowed := allowedTypes(r.Options.AlbumTypes)
	latest := map[string]LBRelease{}
	for _, rel := range releases {
		if !allowed[rel.PrimaryType] || len(rel.ArtistMBIDs) == 0 || rel.ReleaseGroupMBID == "" || rel.ReleaseMBID == "" {
			continue
		}
		mbid := rel.ArtistMBIDs[0]
		if mbid == variousArtistsMBID || !mbidPattern.MatchString(mbid) || index.ArtistPresent(mbid, rel.ArtistCredit) {
			continue
		}
		if prev, ok := latest[mbid]; !ok || rel.ReleaseDate > prev.ReleaseDate {
			latest[mbid] = rel
		}
	}
	artists := make([]string, 0, len(latest))
	for mbid := range latest {
		artists = append(artists, mbid)
	}
	sort.Strings(artists)

	// 2. Their nearest artists, cached across runs.
	for mbid, entry := range r.State.Neighbours {
		if now.Sub(entry.Checked) > neighboursTTL {
			delete(r.State.Neighbours, mbid)
		}
	}
	seedPlays := map[string]int{}
	for _, seed := range r.State.Seeds {
		seedPlays[artistIdentity(seed.Name)] = seed.Plays
	}
	type match struct {
		mbid   string
		owned  int
		weight int
	}
	var matches []match
	fetched, failed, known := 0, 0, 0
	for _, mbid := range artists {
		entry, ok := r.State.Neighbours[mbid]
		if !ok {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			entry, err = r.neighbours(ctx, mbid, now)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				failed++
				continue
			}
			r.State.Neighbours[mbid] = entry
			if fetched++; fetched%neighboursCheckpoint == 0 {
				logf("new artists: %d/%d looked up on ListenBrainz", fetched, len(artists))
				if err := r.State.Save(); err != nil {
					logf("state save: %v", err)
				}
			}
		}
		if len(entry.IDs) > 0 {
			known++
		}
		m := match{mbid: mbid}
		for i, id := range entry.IDs {
			if index.ArtistPresent(id, entry.Names[i]) {
				m.owned++
				m.weight += seedPlays[artistIdentity(entry.Names[i])]
			}
		}
		if m.owned > 0 {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].owned != matches[j].owned {
			return matches[i].owned > matches[j].owned
		}
		if matches[i].weight != matches[j].weight {
			return matches[i].weight > matches[j].weight
		}
		return matches[i].mbid < matches[j].mbid
	})

	// 3. New means a debut inside the window, checked only for the matches the run can
	// try: one MusicBrainz search per artist.
	limit := r.Options.MaxAlbums * triesPerAlbum
	cutoff := now.Add(-r.Options.NewArtists.Debut)
	var out []candidate
	older := 0
	for _, m := range matches {
		if limit > 0 && len(out) >= limit {
			break
		}
		rel := latest[m.mbid]
		groups, err := r.Brainz.ArtistAlbums(ctx, m.mbid, r.Options.AlbumTypes)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logf("new artist %s: %v", rel.ArtistCredit, err)
			continue
		}
		if !isNewArtist(groups, rel.ReleaseGroupMBID, cutoff) {
			older++
			continue
		}
		out = append(out, candidate{
			Artist: rel.ArtistCredit, ArtistMBID: m.mbid, Title: rel.ReleaseName,
			RGID: rel.ReleaseGroupMBID, ReleaseID: rel.ReleaseMBID, rank: len(out),
		})
	}
	logf("new artists: %d releases of the last %d days, %d artists you do not have (%d known to ListenBrainz, %d lookups failed), %d close to yours, %d debuted before %s, %d new",
		len(releases), days, len(artists), known, failed, len(matches), older, cutoff.Format("2006-01-02"), len(out))
	return out, nil
}

// neighbours asks ListenBrainz for the nearest artists of one artist, itself excluded.
func (r *DiscoverRunner) neighbours(ctx context.Context, mbid string, now time.Time) (artistNeighbours, error) {
	tracks, err := r.Listen.RadioArtist(ctx, mbid, nearestArtists)
	if err != nil {
		return artistNeighbours{}, err
	}
	entry := artistNeighbours{Checked: now}
	seen := map[string]bool{mbid: true}
	for _, t := range tracks {
		if t.ArtistMBID == "" || seen[t.ArtistMBID] {
			continue
		}
		seen[t.ArtistMBID] = true
		entry.IDs = append(entry.IDs, t.ArtistMBID)
		entry.Names = append(entry.Names, t.ArtistName)
	}
	return entry, nil
}

// isNewArtist reports whether the artist's first studio album or EP came out after the
// cutoff, and that the release it is offered for is not itself a live or compilation
// group. A partial date counts from its first day, so a bare year never hides an old
// debut.
func isNewArtist(groups []ArtistReleaseGroup, rgID string, cutoff time.Time) bool {
	for _, g := range groups {
		if g.ID == rgID && len(g.SecondaryTypes) > 0 {
			return false
		}
		if len(g.SecondaryTypes) > 0 || g.FirstReleaseDate == "" {
			continue
		}
		if first, ok := parsePartialDate(g.FirstReleaseDate); ok && first.Before(cutoff) {
			return false
		}
	}
	return true
}

func parsePartialDate(value string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
