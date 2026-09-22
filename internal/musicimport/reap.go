package musicimport

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReapOptions is the policy applied to the reachability facts. The engine only
// records them; the threshold lives here, where it can be tuned without a rebuild.
type ReapOptions struct {
	MinFailures int
	MinSpan     time.Duration
	Limit       int
	Apply       bool
}

// ReapSummary is one reap pass.
type ReapSummary struct {
	Albums        int
	Candidates    int
	Removed       int
	Files         int
	SkippedActive int
	Notes         []string
}

// Reap removes albums whose swarm has been unreachable for long enough. An album is
// one torrent (measured 1:1 in the production library), so the per-hash counter the
// engine already keeps is the album's counter. One success deletes that counter, so
// an album that revives restarts its own window from zero and cannot be condemned by
// failures accumulated across separate outages.
func (r *Runner) Reap(ctx context.Context, opts ReapOptions) (ReapSummary, error) {
	var summary ReapSummary
	rows, err := r.Library.AudioRows(ctx)
	if err != nil {
		return summary, fmt.Errorf("read the library: %w", err)
	}
	albums := groupAlbums(rows)
	summary.Albums = len(albums)

	type candidate struct {
		prefix string
		count  int64
	}
	var candidates []candidate
	for _, album := range albums {
		if album.prefix == "" {
			continue
		}
		// An open session has not acquitted its counter yet: the album may be playing
		// right now and would look condemned for as long as the file stays open.
		if album.activeSession {
			summary.SkippedActive++
			continue
		}
		span := time.Duration(album.lastFailNS - album.firstFailNS)
		if album.failCount < int64(opts.MinFailures) || span < opts.MinSpan {
			continue
		}
		candidates = append(candidates, candidate{prefix: album.prefix, count: album.failCount})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].count != candidates[j].count {
			return candidates[i].count > candidates[j].count
		}
		return candidates[i].prefix < candidates[j].prefix
	})
	summary.Candidates = len(candidates)
	if opts.Limit > 0 && len(candidates) > opts.Limit {
		candidates = candidates[:opts.Limit]
	}

	for _, candidate := range candidates {
		if !opts.Apply {
			summary.Notes = append(summary.Notes, fmt.Sprintf("would reap %s (%d failures)", candidate.prefix, candidate.count))
			continue
		}
		result, err := r.Library.RemovePrefix(ctx, candidate.prefix)
		if err != nil {
			summary.Notes = append(summary.Notes, fmt.Sprintf("reap %s: %v", candidate.prefix, err))
			continue
		}
		summary.Removed++
		summary.Files += result.Removed
		summary.Notes = append(summary.Notes, fmt.Sprintf("reaped %s (%d projections, torrent referenced: %t)",
			candidate.prefix, result.Removed, result.TorrentReferenced))
	}
	return summary, nil
}

type albumFailures struct {
	prefix        string
	hash          string
	failCount     int64
	firstFailNS   int64
	lastFailNS    int64
	activeSession bool
	rows          []AudioRow
}

// groupAlbums folds the flat projection rows into albums keyed by hash, the torrent
// that owns them. The prefix is the deepest directory every row of the album shares,
// which covers both layouts in the library: Artist/Album/... from the importer and
// the single-folder releases added by hand.
func groupAlbums(rows []AudioRow) []albumFailures {
	byHash := map[string][]AudioRow{}
	var order []string
	for _, row := range rows {
		if row.Hash == "" {
			continue
		}
		if _, seen := byHash[row.Hash]; !seen {
			order = append(order, row.Hash)
		}
		byHash[row.Hash] = append(byHash[row.Hash], row)
	}

	albums := make([]albumFailures, 0, len(order))
	for _, hash := range order {
		group := byHash[hash]
		album := albumFailures{hash: hash, prefix: commonDirPrefix(group), rows: group}
		for _, row := range group {
			if row.ActiveSession {
				album.activeSession = true
			}
			if row.FailCount > album.failCount {
				album.failCount = row.FailCount
			}
			if album.firstFailNS == 0 || row.FirstFailNS < album.firstFailNS {
				album.firstFailNS = row.FirstFailNS
			}
			if row.LastFailNS > album.lastFailNS {
				album.lastFailNS = row.LastFailNS
			}
		}
		albums = append(albums, album)
	}
	return albums
}

// commonDirPrefix returns the deepest directory shared by every path, component-wise.
func commonDirPrefix(rows []AudioRow) string {
	var common []string
	for i, row := range rows {
		dir := row.Path
		if cut := strings.LastIndexByte(dir, '/'); cut >= 0 {
			dir = dir[:cut]
		} else {
			dir = ""
		}
		parts := strings.Split(dir, "/")
		if i == 0 {
			common = parts
			continue
		}
		limit := len(parts)
		if len(common) < limit {
			limit = len(common)
		}
		match := 0
		for match < limit && parts[match] == common[match] {
			match++
		}
		common = common[:match]
	}
	if len(common) == 0 {
		return ""
	}
	return strings.Join(common, "/")
}
