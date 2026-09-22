package musicimport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Seed is one artist the listening points at, with the plays that earned it.
type Seed struct {
	Name  string `json:"name"`
	MBID  string `json:"mbid"`
	Plays int    `json:"plays"`
}

// SeedOptions are the knobs of the seed pass.
type SeedOptions struct {
	Count    int
	MinPlays int
	Windows  []time.Duration // shortest first; zero means all-time
}

// historySource is the slice of Plex the seed pass needs.
type historySource interface {
	History(ctx context.Context, after time.Time) ([]Play, error)
	Artists(ctx context.Context, section string) ([]Artist, error)
}

// artistSearcher resolves the names Plex never matched.
type artistSearcher interface {
	SearchArtist(ctx context.Context, name string) (string, bool, error)
}

// collectSeeds walks the windows shortest-first and stops at the first whose names
// resolve to a full seed set. Every window keeps the best resolved result seen so
// far: a thin week falls back to a wider one instead of shrinking the run. The cap
// applies AFTER the resolution, so an unresolvable name never eats the place of the
// resolvable artists behind it; a name is resolved once across windows.
func collectSeeds(ctx context.Context, src historySource, brainz artistSearcher, sections []string, opts SeedOptions, now time.Time, logf func(string, ...any)) ([]Seed, time.Duration, error) {
	if len(opts.Windows) == 0 {
		return nil, 0, errors.New("seed windows are empty: check the discovery config")
	}
	var best []Seed
	var bestWindow time.Duration
	var index map[string]string
	memo := map[string]string{} // name -> mbid ("" = unresolvable), across windows
	for _, window := range opts.Windows {
		after := time.Time{}
		if window > 0 {
			after = now.Add(-window)
		}
		plays, err := src.History(ctx, after)
		if err != nil {
			return nil, 0, fmt.Errorf("plex history: %w", err)
		}
		seeds := topArtists(plays, opts.MinPlays, 0)
		logf("window %s: %d plays, %d seeds", windowLabel(window), len(plays), len(seeds))
		if len(seeds) <= len(best) {
			continue
		}
		if index == nil {
			if index, err = artistMBIDs(ctx, src, sections); err != nil {
				return nil, window, err
			}
		}
		resolved := fillSeedMBIDs(ctx, seeds, index, brainz, memo, opts.Count, logf)
		if len(resolved) > len(best) {
			best, bestWindow = resolved, window
		}
		if opts.Count > 0 && len(best) >= opts.Count {
			break
		}
	}
	return best, bestWindow, nil
}

// topArtists aggregates the plays per artist identity and keeps the ones above the
// floor. Two spellings of the same name are one artist; a featuring credit is not a
// different artist; an identical row repeated by Plex is one play.
func topArtists(plays []Play, minPlays, limit int) []Seed {
	type aggregate struct {
		total    int
		spelling map[string]int
	}
	byIdentity := map[string]*aggregate{}
	seen := map[string]bool{}
	for _, p := range plays {
		if p.Artist == "" {
			continue
		}
		row := p.Artist + "\x00" + p.Album + "\x00" + p.Title + "\x00" + strconv.FormatInt(p.ViewedAt.Unix(), 10)
		if seen[row] {
			continue
		}
		seen[row] = true
		identity := artistIdentity(p.Artist)
		if identity == "" {
			// A punctuation-only name has no comparable identity: it stays its own
			// artist instead of merging with every other nameless entry.
			identity = "\x00raw\x00" + p.Artist
		}
		a := byIdentity[identity]
		if a == nil {
			a = &aggregate{spelling: map[string]int{}}
			byIdentity[identity] = a
		}
		a.total++
		a.spelling[p.Artist]++
	}
	seeds := make([]Seed, 0, len(byIdentity))
	for _, a := range byIdentity {
		if a.total < minPlays {
			continue
		}
		seeds = append(seeds, Seed{Name: dominantSpelling(a.spelling), Plays: a.total})
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].Plays != seeds[j].Plays {
			return seeds[i].Plays > seeds[j].Plays
		}
		return seeds[i].Name < seeds[j].Name
	})
	if limit > 0 && len(seeds) > limit {
		seeds = seeds[:limit]
	}
	return seeds
}

// dominantSpelling picks the spelling the plays used most, ties broken
// alphabetically, so the displayed seed name is stable and readable.
func dominantSpelling(spellings map[string]int) string {
	best, bestCount := "", -1
	for name, count := range spellings {
		if count > bestCount || (count == bestCount && name < best) {
			best, bestCount = name, count
		}
	}
	return best
}

// artistMBIDs indexes the names Plex already matched in every section, keyed by
// identity, so the seed pass rarely needs MusicBrainz: the plays come from all the
// sections. The first section listed wins a name two sections disagree on. A name
// with no identity is not indexed: the empty key would collide with every other.
func artistMBIDs(ctx context.Context, src historySource, sections []string) (map[string]string, error) {
	index := map[string]string{}
	for _, section := range sections {
		artists, err := src.Artists(ctx, section)
		if err != nil {
			return nil, fmt.Errorf("plex artists %s: %w", section, err)
		}
		for _, a := range artists {
			if a.MBID == "" {
				continue
			}
			key := artistIdentity(a.Name)
			if key == "" {
				continue
			}
			if _, ok := index[key]; !ok {
				index[key] = a.MBID
			}
		}
	}
	return index, nil
}

// nameKey is the identity of a name for the fallback match: case, punctuation and
// stopwords are not identity ("SOFIA ISELLA" == "Sofia Isella"). When the meaningful
// tokens vanish ("Us", "We", "It", "X", "The The") the normalized words themselves
// become the key, under a private prefix so the two schemes never collide: an empty
// key would silently disable the name match and let a duplicate through.
func nameKey(name string) string {
	if tokens := normalizeTokens(name); len(tokens) > 0 {
		return strings.Join(tokens, " ")
	}
	words := strings.Join(strings.Fields(normalizeTitle(name)), " ")
	if words == "" {
		return ""
	}
	return "\x00words\x00" + words
}

// artistIdentity is nameKey plus the featuring credit: "X feat. Y" is X.
func artistIdentity(name string) string { return nameKey(stripFeaturing(name)) }

// stripFeaturing removes the featuring credit from an artist name. The credit is not
// part of the artist's identity and searching it on MusicBrainz finds nothing.
func stripFeaturing(name string) string {
	lower := strings.ToLower(name)
	for _, cut := range []string{" feat.", " feat ", " ft.", " ft ", " featuring "} {
		if index := strings.Index(lower, cut); index > 0 {
			name = name[:index]
			lower = lower[:index]
		}
	}
	return strings.TrimSpace(name)
}

// fillSeedMBIDs resolves the seeds the Plex index misses, one search at a time, and
// stops once limit seeds are resolved (0 = no limit); the seeds arrive strongest
// first. A seed whose name has no identity, or that cannot be resolved, is dropped:
// without the id there is no radio. memo (optional) remembers searches across calls.
func fillSeedMBIDs(ctx context.Context, seeds []Seed, index map[string]string, brainz artistSearcher, memo map[string]string, limit int, logf func(string, ...any)) []Seed {
	kept := make([]Seed, 0, len(seeds))
	for _, seed := range seeds {
		if limit > 0 && len(kept) >= limit {
			break
		}
		key := artistIdentity(seed.Name)
		if key == "" {
			logf("artist %q has no comparable name, dropped from the seeds", seed.Name)
			continue
		}
		if mbid := index[key]; mbid != "" {
			seed.MBID = mbid
			kept = append(kept, seed)
			continue
		}
		if mbid, seen := memo[key]; seen {
			if mbid != "" {
				seed.MBID = mbid
				kept = append(kept, seed)
			}
			continue
		}
		if brainz == nil {
			logf("artist %s has no MusicBrainz id, dropped from the seeds", seed.Name)
			continue
		}
		mbid, ok, err := brainz.SearchArtist(ctx, stripFeaturing(seed.Name))
		if err != nil {
			logf("artist lookup %s: %v", seed.Name, err)
		} else if memo != nil {
			memo[key] = mbid
		}
		if !ok {
			logf("artist %s has no MusicBrainz id, dropped from the seeds", seed.Name)
			continue
		}
		seed.MBID = mbid
		kept = append(kept, seed)
	}
	return kept
}

func windowLabel(window time.Duration) string {
	if window <= 0 {
		return "all-time"
	}
	return fmt.Sprintf("%dd", int(window.Hours()/24))
}
