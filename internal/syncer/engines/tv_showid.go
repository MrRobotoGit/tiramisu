package engines

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/metadb"
)

// ErrShowIdentityUnclear is returned when the id stored on the episodes and the one
// the name search resolves to disagree. Acting on either would mean re-searching one
// show while deciding the fate of another's files, so the caller must not act at all.
var ErrShowIdentityUnclear = errors.New("stored show id disagrees with the name search")

// reShowFolderYear splits "A_Good_Girls_Guide_to_Murder (2024)" into name and year.
var reShowFolderYear = regexp.MustCompile(`^(.*?)\s*\((\d{4})\)$`)

// reSeasonDir matches the season directory the TV sync writes, e.g. "Season.01".
var reSeasonDir = regexp.MustCompile(`(?i)^season[._ ]?\d{1,3}$`)

// showQueryFromPath recovers a searchable show name and year from an episode path.
// The episode key is lowercased with every non-word character stripped, so it is a
// poor query; the show folder keeps the readable name and the first-air year.
func showQueryFromPath(episodePath string) (string, string) {
	// <tv>/<Show Folder>/Season.NN/<episode>.mkv
	seasonDir := filepath.Dir(episodePath)
	// The layout has to be the real one: on a flat or manually placed file the parent
	// of the parent is the library root, and searching TMDB for "tv" returns an
	// arbitrary show that would then be written onto every episode of the set.
	if !reSeasonDir.MatchString(filepath.Base(seasonDir)) {
		return "", ""
	}
	folder := filepath.Base(filepath.Dir(seasonDir))
	if folder == "." || folder == string(filepath.Separator) || folder == "" {
		return "", ""
	}
	year := ""
	if m := reShowFolderYear.FindStringSubmatch(folder); len(m) == 3 {
		folder, year = m[1], m[2]
	}
	return strings.TrimSpace(strings.ReplaceAll(folder, "_", " ")), year
}

// ShowNameFromEpisodePath is showQueryFromPath without the year, for callers that
// only need the readable name.
func ShowNameFromEpisodePath(episodePath string) string {
	name, _ := showQueryFromPath(episodePath)
	return name
}

// resolveShow finds the TMDB show behind a set of episodes and makes sure their rows
// carry its IMDB id. Episodes registered before the id was stored have none, and the
// id is what lets a dead release be re-searched: TVExternalIDs needs the numeric TMDB
// id, so the name is the bridge back to it.
//
// Resolution is lazy on purpose: only the shows with something to decide pay for it,
// and the id is persisted so the next run does not ask again.
func (e *TVGoEngine) resolveShow(ctx context.Context, entries []metadb.EpisodeEntry) (tmdb.TVShow, string, error) {
	if len(entries) == 0 {
		return tmdb.TVShow{}, "", fmt.Errorf("no episodes to resolve")
	}

	stored := ""
	for _, ep := range entries {
		if ep.ShowIMDB != "" {
			stored = ep.ShowIMDB
			break
		}
	}

	// The first path that yields a name, not simply the first entry: the set has no
	// meaningful order, and one unusable path should not decide for the rest.
	name, year := "", ""
	for _, ep := range entries {
		if n, y := showQueryFromPath(ep.FilePath); n != "" {
			name, year = n, y
			break
		}
	}
	if name == "" {
		return tmdb.TVShow{}, stored, fmt.Errorf("cannot read a show name from %d episode path(s)", len(entries))
	}

	show, err := e.tmdb.SearchTVBest(ctx, name, year)
	if err != nil {
		return tmdb.TVShow{}, stored, fmt.Errorf("search %q: %w", name, err)
	}

	imdbID, err := e.tmdb.TVExternalIDs(ctx, show.ID)
	if err != nil || imdbID == "" {
		// The show was found but has no external id: usable for a re-search, not for
		// persisting an identity.
		return show, stored, nil
	}

	// A stored id that disagrees with the search is the search being wrong, not the
	// record: names are ambiguous, ids are not.
	if stored != "" && stored != imdbID {
		e.logger.Printf("[TVSync] %s resolves to %s but its episodes carry %s: identity unclear, not acting", name, imdbID, stored)
		// Written to the rows that still lack it, or the same search runs again next time.
		e.persistShowIMDB(entries, stored)
		// The show returned by the search belongs to the other id: handing it back with
		// the stored one would re-search one show to decide about another's files.
		return tmdb.TVShow{}, stored, ErrShowIdentityUnclear
	}

	e.persistShowIMDB(entries, imdbID)
	return show, imdbID, nil
}

// persistShowIMDB fills in the id for the episodes that lack it. The entries must be
// whole rows as read back from the registry: the write replaces the record, so a
// partial entry would blank hash, source and created.
func (e *TVGoEngine) persistShowIMDB(entries []metadb.EpisodeEntry, imdbID string) {
	if e.db == nil || imdbID == "" {
		return
	}
	for _, ep := range entries {
		if ep.ShowIMDB == imdbID {
			continue
		}
		if ep.Hash == "" {
			// A partial entry: writing it would replace a real row with an empty one,
			// or resurrect an episode that no longer exists. The gap records carry the
			// id themselves, so nothing is lost by skipping.
			continue
		}
		ep.ShowIMDB = imdbID
		if err := e.db.UpsertEpisode(ep.EpisodeKey, ep); err != nil {
			// One row failing (a busy DB, say) must not cost the whole set: the
			// unwritten ones would send the search back to TMDB on the next run.
			e.logger.Printf("[TVSync] Warning: could not store the show id for %s: %v", ep.EpisodeKey, err)
			continue
		}
	}
}
