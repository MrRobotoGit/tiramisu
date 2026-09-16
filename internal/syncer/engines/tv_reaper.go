package engines

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// deadPack is one torrent that stopped answering, with every episode that depends
// on it. A season pack stands behind many episodes, so the unit of decision is the
// hash, never the single file.
type deadPack struct {
	Hash     string
	Episodes []metadb.EpisodeEntry
	Seasons  []int
}

// flagDeadPacks marks the episodes whose torrent stopped resolving its metadata and
// returns them grouped by release. The episodes stay registered: deregistering would
// have cleanupOrphanedFiles delete their stubs at the end of the run. What changes is
// that every quality comparison now treats them as absent (see isDeadEpisode), so a
// replacement of equal quality is no longer refused.
func (e *TVGoEngine) flagDeadPacks() []deadPack {
	if e.db == nil {
		return nil
	}
	hashes, err := e.db.MetadataFailuresOver(deadReleaseFailures, deadReleaseSpan)
	if err != nil {
		e.logger.Printf("[TVSync] WARNING: could not read metadata failures: %v", err)
		return nil
	}

	var packs []deadPack
	if e.deadPackHashes == nil {
		e.deadPackHashes = make(map[string]bool)
	}
	if e.deadEpisodeKeys == nil {
		e.deadEpisodeKeys = make(map[string]bool)
	}
	for _, hash := range hashes {
		episodes, err := e.db.EpisodesByHash(hash)
		if err != nil {
			e.logger.Printf("[TVSync] WARNING: could not list episodes of %s: %v", hash[:8], err)
			continue
		}
		if len(episodes) == 0 {
			// A movie, or a pack already replaced: its row would otherwise be re-read
			// on every run for good. The movie engine keeps its own counters.
			continue
		}
		// Excluded from this run's candidates right away, not only in the reaper pass:
		// the discovery loop would otherwise retry the silent release once, at the cost
		// of a full metadata wait.
		e.deadPackHashes[strings.ToLower(hash)] = true

		seasons := make(map[int]bool)
		for _, ep := range episodes {
			e.deadEpisodeKeys[ep.EpisodeKey] = true
			if sn := seasonFromEpisodeKey(ep.EpisodeKey); sn > 0 {
				seasons[sn] = true
			}
		}
		pack := deadPack{Hash: hash, Episodes: episodes}
		for sn := range seasons {
			pack.Seasons = append(pack.Seasons, sn)
		}
		sort.Ints(pack.Seasons)
		packs = append(packs, pack)

		e.logger.Printf("[TVSync] Dead release %s: %d episode(s) over season(s) %v, re-searching",
			hash[:8], len(episodes), pack.Seasons)
	}
	return packs
}

// seasonFromEpisodeKey reads the season out of "<show>_sXXeYY", 0 when the key does
// not carry one.
func seasonFromEpisodeKey(key string) int {
	i := strings.LastIndex(key, "_s")
	if i < 0 || len(key) < i+5 {
		return 0
	}
	rest := key[i+2:]
	j := strings.Index(rest, "e")
	if j <= 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0
	}
	return n
}

// reapDeadPacks gives each dead release its own search and drops the stubs nothing
// could replace. It runs after the discovery loop, because discovery only returns
// recent and trending shows: a pack that died on an old season is reachable no other
// way.
func (e *TVGoEngine) reapDeadPacks(ctx context.Context, packs []deadPack) {
	for _, pack := range packs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if len(pack.Seasons) == 0 {
			// Nothing to scope a search with: dropReplacedPack would refuse anyway, so
			// spend nothing on it.
			e.logger.Printf("[TVSync] Dead release %s: no season could be read from its keys, keeping every stub", pack.Hash[:8])
			continue
		}

		show, _, err := e.resolveShow(ctx, pack.Episodes)
		if err != nil {
			// An unclear identity is not a failure to retry: acting on it would mean
			// re-searching one show to decide about another's files.
			if errors.Is(err, ErrShowIdentityUnclear) {
				e.logger.Printf("[TVSync] Dead release %s: show identity unclear, keeping every stub", pack.Hash[:8])
			} else {
				e.logger.Printf("[TVSync] Dead release %s: cannot resolve the show (%v), keeping every stub", pack.Hash[:8], err)
			}
			continue
		}

		searched := e.processShow(ctx, show, pack.Seasons...)
		e.dropReplacedPack(ctx, pack, searched)
	}
}

// dropReplacedPack removes the stubs still pointing at the dead release, but only
// when every season it covers was conclusively searched. A season nobody could reach
// says nothing about the release, and a pack is up to twenty episodes: the cost of
// being wrong is a whole season, so an inconclusive run keeps everything.
func (e *TVGoEngine) dropReplacedPack(ctx context.Context, pack deadPack, searched map[int]seasonSearch) {
	if len(pack.Seasons) == 0 {
		// Nothing to check means nothing was established: without a season the gate
		// below would be vacuously true and the pack would go on a default-window search.
		e.logger.Printf("[TVSync] Dead release %s: no season could be read from its keys, keeping every stub", pack.Hash[:8])
		return
	}
	// First usable path, like resolveShow: one unreadable path must not blank the name
	// in the very line the weekly review is read from.
	show := ""
	for _, ep := range pack.Episodes {
		if n, _ := showQueryFromPath(ep.FilePath); n != "" {
			show = n
			break
		}
	}
	for _, season := range pack.Seasons {
		got := searched[season]
		if !got.Complete {
			e.logger.Printf("[TVSync] Dead release %s: season %d was not conclusively searched, keeping every stub",
				pack.Hash[:8], season)
			return
		}
		// Policy for TV, deliberately stricter than for movies: a season searched end
		// to end that produced no release at all is indistinguishable from an indexer
		// answering 200 with an empty list, and a pack is up to twenty episodes. A
		// movie stub is one file, so there the same case allows removal; here the cost
		// of being wrong is a whole season of watch state, and the cost of waiting is
		// the metadata timeouts Plex keeps paying on the stub.
		//
		// The line below is the review input: count these after a week. Always the same
		// few seasons means the releases really are gone and this policy stays; many and
		// varying means the indexers are degrading and the signal needs work.
		if !got.RawSeen {
			// Episodes of this season, not of the whole pack: a S01-S02 pack would
			// otherwise report the same total twice and hide what each season costs.
			inSeason := 0
			for _, ep := range pack.Episodes {
				if seasonFromEpisodeKey(ep.EpisodeKey) == season {
					inSeason++
				}
			}
			e.logger.Printf("[TVSync] Dead release %s (%s S%02d, %d of %d episode(s)): both sources returned nothing for this season, keeping every stub",
				pack.Hash[:8], show, season, inSeason, len(pack.Episodes))
			return
		}
	}

	// Only the seasons the gate above actually verified may lose stubs. An episode whose
	// key carries no readable season was never searched, so it is kept.
	verified := make(map[int]bool, len(pack.Seasons))
	for _, sn := range pack.Seasons {
		verified[sn] = true
	}

	removed := 0
	kept := 0
	for _, ep := range pack.Episodes {
		if !verified[seasonFromEpisodeKey(ep.EpisodeKey)] {
			kept++
			continue
		}
		// Re-read: the search above may have replaced this episode, in which case the
		// registry now points at another release and there is nothing to remove.
		current, ok := e.registry[ep.EpisodeKey]
		if !ok || !strings.EqualFold(current.Hash, pack.Hash) {
			continue
		}
		// The torrent is shared by every episode of the pack, so it is dropped once,
		// after the loop: removing it here would fail loudly on all the others.
		e.removeStubFile(current.FilePath)
		delete(e.registry, ep.EpisodeKey)
		if e.db != nil {
			if err := e.db.DeleteEpisode(ep.EpisodeKey); err != nil {
				e.logger.Printf("[TVSync] Warning: could not deregister %s: %v", ep.EpisodeKey, err)
			}
			// Remembered, not just removed: discovery returns a fraction of the library,
			// so a season it never reaches would keep this hole forever.
			if err := e.db.RecordEpisodeGap(metadb.EpisodeGap{
				EpisodeKey: ep.EpisodeKey,
				ShowIMDB:   ep.ShowIMDB,
				Season:     seasonFromEpisodeKey(ep.EpisodeKey),
				FilePath:   current.FilePath,
				DeadHash:   pack.Hash,
			}); err != nil {
				e.logger.Printf("[TVSync] Warning: could not record the gap for %s: %v", ep.EpisodeKey, err)
			}
		}
		removed++
	}
	if removed > 0 {
		// One call for the whole pack: every episode points at the same torrent. A stub
		// kept above still needs it, so the torrent only goes when none is left.
		if kept == 0 {
			if err := e.gostorm.RemoveTorrent(ctx, pack.Hash); err != nil {
				e.logger.Printf("[TVSync] WARNING: failed to remove torrent %s: %v", pack.Hash[:8], err)
			}
		}
		e.logger.Printf("[TVSync] No live release for %s (%s season(s) %v): removed %d stub(s), recorded as gaps",
			pack.Hash[:8], show, pack.Seasons, removed)
	}
	if kept > 0 {
		e.logger.Printf("[TVSync] Dead release %s: kept %d stub(s) whose season could not be read from the key, torrent left in place",
			pack.Hash[:8], kept)
	}
	// The counter outlives the release otherwise: the row stays and is re-read every
	// run, and a hash picked again later would arrive already condemned.
	if e.db != nil {
		if err := e.db.ClearMetadataFailure(pack.Hash); err != nil {
			e.logger.Printf("[TVSync] Warning: could not clear the failure counter for %s: %v", pack.Hash[:8], err)
		}
	}
}

const (
	// gapRetryAfter keeps a fresh hole out of the repair pass: it was just created by
	// a search that failed on the same season with the same sources, so retrying it in
	// the same run costs a full season search to learn nothing.
	gapRetryAfter = 6 * time.Hour
	// gapGroupsPerRun bounds the work a backlog can add to one sync. EpisodeGaps is
	// oldest-first, so the queue still advances in order.
	gapGroupsPerRun = 5
)

// repairEpisodeGaps re-searches the seasons the reaper emptied. It is the other half
// of removing: discovery reaches 88 shows out of 437 here, so without this pass a
// hole in a show it does not return would never be filled, whatever appears in the
// swarms later.
func (e *TVGoEngine) repairEpisodeGaps(ctx context.Context) {
	if e.db == nil {
		return
	}
	gaps, err := e.db.EpisodeGaps()
	if err != nil {
		e.logger.Printf("[TVSync] WARNING: could not read the episode gaps: %v", err)
		return
	}
	if len(gaps) == 0 {
		return
	}

	// Grouped by show, seasons inside: the show is resolved once, and one search per
	// season covers every hole in it.
	type showGaps struct {
		name    string
		seasons map[int][]metadb.EpisodeGap
	}
	cutoff := time.Now().Add(-gapRetryAfter).Unix()
	order := make([]string, 0, len(gaps))
	byShow := make(map[string]*showGaps)
	for _, g := range gaps {
		if e.registry[g.EpisodeKey].FilePath != "" {
			// Already back, by this run or another: the record is stale.
			_ = e.db.ClearEpisodeGap(g.EpisodeKey)
			continue
		}
		// The throttle has to look at the last attempt, not only at the removal: a hole
		// open for weeks would otherwise be re-searched on every single run, which is
		// the unbounded cost gapRetryAfter exists to prevent.
		last := g.RemovedAt
		if g.LastAttempt > last {
			last = g.LastAttempt
		}
		if last > cutoff || g.Season <= 0 {
			// Too recently tried, or no season to scope a search with.
			continue
		}
		name, _ := showQueryFromPath(g.FilePath)
		if name == "" {
			// Unrepairable as recorded. Stamp it anyway, or it sits at the head of the
			// oldest-first queue for good and hides the gaps that can be repaired.
			if err := e.db.MarkGapAttempted(g.EpisodeKey); err != nil {
				e.logger.Printf("[TVSync] Warning: could not stamp the unreadable gap %s: %v", g.EpisodeKey, err)
			}
			continue
		}
		sg, ok := byShow[name]
		if !ok {
			sg = &showGaps{name: name, seasons: make(map[int][]metadb.EpisodeGap)}
			byShow[name] = sg
			order = append(order, name)
		}
		sg.seasons[g.Season] = append(sg.seasons[g.Season], g)
	}

	done := 0
	for _, name := range order {
		if done >= gapGroupsPerRun {
			e.logger.Printf("[TVSync] %d show(s) with open gaps left for the next run", len(order)-done)
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		sg := byShow[name]

		var any []metadb.EpisodeGap
		for _, group := range sg.seasons {
			any = append(any, group...)
		}
		entries := make([]metadb.EpisodeEntry, 0, len(any))
		for _, g := range any {
			// Hash stays empty: the episode is gone and no release stands behind it any
			// more. The entry carries only what resolveShow needs to find the show, and
			// storing the id is a single-column update, so no row is written back.
			entries = append(entries, metadb.EpisodeEntry{
				EpisodeKey: g.EpisodeKey, FilePath: g.FilePath, ShowIMDB: g.ShowIMDB,
			})
		}
		// Stamped before the outcome is known: a show that cannot be resolved must move
		// to the back of the queue, or it is retried first on every run and the rest of
		// the backlog is never reached.
		for _, g := range any {
			if err := e.db.MarkGapAttempted(g.EpisodeKey); err != nil {
				e.logger.Printf("[TVSync] Warning: could not stamp the gap %s: %v", g.EpisodeKey, err)
			}
		}

		show, _, err := e.resolveShow(ctx, entries)
		if err != nil {
			e.logger.Printf("[TVSync] Gaps on %s: cannot resolve the show (%v)", name, err)
			done++
			continue
		}

		for season, group := range sg.seasons {
			// The release that made the hole is excluded again. deadPackHashes is rebuilt
			// every run and the failure counter was cleared on removal, so without this
			// the search happily re-picks the dead pack and the cycle starts over.
			if e.deadPackHashes == nil {
				e.deadPackHashes = make(map[string]bool)
			}
			for _, g := range group {
				if g.DeadHash != "" {
					e.deadPackHashes[strings.ToLower(g.DeadHash)] = true
				}
			}
			e.logger.Printf("[TVSync] %d gap(s) open on %s S%02d: re-searching", len(group), name, season)
			e.processShow(ctx, show, season)

			filled := 0
			for _, g := range group {
				if e.registry[g.EpisodeKey].FilePath == "" {
					continue
				}
				if err := e.db.ClearEpisodeGap(g.EpisodeKey); err != nil {
					e.logger.Printf("[TVSync] Warning: could not clear the gap for %s: %v", g.EpisodeKey, err)
					continue
				}
				filled++
			}
			if filled > 0 {
				e.logger.Printf("[TVSync] %d gap(s) filled on %s S%02d", filled, name, season)
			}
		}
		done++
	}
}
