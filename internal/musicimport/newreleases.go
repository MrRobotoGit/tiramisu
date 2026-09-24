package musicimport

import (
	"context"
	"regexp"
	"sort"
	"time"
)

// NewReleaseOptions turns on the follow of the library's artists: every album they
// released inside the window and the library lacks is imported, with no cap.
type NewReleaseOptions struct {
	Enabled bool
	Window  time.Duration
	// Section is the library Tiramisu files music into: the follow reads no other.
	Section Section
}

// OwnMusicSection finds Tiramisu's music library on a server that has no configured
// id (Jellyfin): the one holding the most albums Tiramisu has filed. None when no
// library holds any.
func OwnMusicSection(ctx context.Context, media albumSource, sections []Section, committed CommittedSet) (Section, bool, error) {
	var best Section
	bestCount := 0
	for _, section := range sections {
		albums, err := media.Albums(ctx, section.Key)
		if err != nil {
			return Section{}, false, err
		}
		count := 0
		for _, a := range albums {
			if committed.HasAlbum(a.Artist, a.Title, "") {
				count++
			}
		}
		if count > bestCount {
			best, bestCount = section, count
		}
	}
	return best, bestCount > 0, nil
}

// variousArtistsMBID is MusicBrainz's placeholder credit for compilations: following
// it would import every compilation of the month.
const variousArtistsMBID = "89ad4ac3-39f7-470e-963a-56509c546377"

// newReleaseBatch is how many artists one MusicBrainz search covers: forty ids keep
// the query URL around 2KB.
const newReleaseBatch = 40

// mbidPattern is the shape of a MusicBrainz id: anything else would break the query.
var mbidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newReleaseCandidates asks MusicBrainz for the albums of the library's artists whose
// first edition came out inside the window, newest first. It needs no memory: the
// window says what is new, the library what is held. A batch whose search fails is
// skipped for this run.
func (r *DiscoverRunner) newReleaseCandidates(ctx context.Context, index *LibraryIndex, now time.Time, logf func(string, ...any)) ([]candidate, error) {
	allowed := allowedTypes(r.Options.AlbumTypes)
	from := now.Add(-r.Options.NewReleases.Window).Truncate(24 * time.Hour)
	var artists []string
	inLibrary := map[string]bool{}
	for _, mbid := range index.ArtistMBIDs() {
		if mbid != variousArtistsMBID && mbidPattern.MatchString(mbid) {
			artists = append(artists, mbid)
			inLibrary[mbid] = true
		}
	}
	seen := map[string]bool{}
	var out []candidate
	failed := 0
	for start := 0; start < len(artists); start += newReleaseBatch {
		batch := artists[start:min(start+newReleaseBatch, len(artists))]
		groups, err := r.Brainz.RecentReleaseGroups(ctx, batch, r.Options.AlbumTypes, from, now)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failed++
			logf("new releases, artists %d-%d: %v", start+1, start+len(batch), err)
			continue
		}
		for _, g := range groups {
			if seen[g.ID] || g.Artist == "" || !allowed[g.PrimaryType] || len(g.SecondaryTypes) > 0 {
				continue
			}
			// The search matches partial dates too; only a full date inside the window counts.
			released, ok := parseReleaseDate(g.FirstReleaseDate)
			if !ok || released.Before(from) || released.After(now) {
				continue
			}
			artistMBID := ""
			for _, id := range g.ArtistMBIDs {
				if inLibrary[id] {
					artistMBID = id
					break
				}
			}
			if artistMBID == "" {
				continue
			}
			seen[g.ID] = true
			out = append(out, candidate{Artist: g.Artist, ArtistMBID: artistMBID, Title: g.Title, RGID: g.ID, released: released, fresh: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].released.Equal(out[j].released) {
			return out[i].released.After(out[j].released)
		}
		return out[i].RGID < out[j].RGID
	})
	logf("new releases: %d albums from the last %d days across %d artists (%d searches failed)",
		len(out), int(r.Options.NewReleases.Window.Hours()/24), len(artists), failed)
	return out, nil
}

// parseReleaseDate accepts a full date only: a bare year or month cannot say whether
// the album falls inside the window.
func parseReleaseDate(value string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", value)
	return t, err == nil
}

// pickRelease chooses the edition the tracklist is read from, the way the listening
// candidates do: official, dated, oldest. Empty when the group has no official
// release yet or MusicBrainz fails; the album is then retried next run.
func (r *DiscoverRunner) pickRelease(ctx context.Context, rgID string, logf func(string, ...any)) string {
	releases, err := r.Brainz.GroupReleases(ctx, rgID)
	if err != nil {
		logf("releases of %s: %v", rgID, err)
		return ""
	}
	var best *RecordingRelease
	for i := range releases {
		if best == nil || betterRelease(releases[i], *best) {
			best = &releases[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.ReleaseID
}
