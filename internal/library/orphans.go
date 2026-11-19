package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OrphanRequest asks for the unreferenced torrents to be listed or released.
type OrphanRequest struct {
	// DryRun defaults to true: nothing is removed unless the caller says false.
	DryRun     *bool `json:"dry_run"`
	Max        int   `json:"max"`          // per run, default 50, at most 1000
	MinAgeDays int   `json:"min_age_days"` // default 7, at least 1
	// CountersOnly clears the stale failure counters and removes no torrent.
	CountersOnly bool `json:"counters_only"`
}

// Orphan is one unreferenced torrent.
type Orphan struct {
	Hash  string `json:"hash"`
	Title string `json:"title"`
	Added string `json:"added"`
}

// OrphanReport says what a collection found and, unless dry, what it did.
type OrphanReport struct {
	DryRun     bool     `json:"dry_run"`
	Candidates int      `json:"candidates"` // all orphans past the age, before Max
	Selected   []Orphan `json:"selected"`
	Released   []string `json:"released"`
	Kept       []string `json:"kept"`   // something cited it on the re-check
	Failed     []string `json:"failed"` // hash: error
	// StaleFailures are metadata failure counters for torrents no longer in the engine,
	// cleared with the run (listed only on a dry run).
	StaleFailures []string `json:"stale_failures"`
	DurationMS    int64    `json:"duration_ms"`
}

const (
	orphanDefaultMax     = 50
	orphanMaxCap         = 1000
	orphanDefaultMinDays = 7
	orphanMaxSharePct    = 25 // more unreferenced than this means a source of references is missing
	orphanBatch          = 50 // candidates released between two reads of the library
)

// CollectOrphans finds the torrents nothing references, from the same content scan the
// consistency check uses, and releases them one by one through ReleaseIfUnused, which
// checks again right before each removal. It refuses to run while a stub cannot be read:
// that stub's torrent cannot be told, and it could be any of the candidates.
func (m *Manager) CollectOrphans(ctx context.Context, req OrphanRequest) (*OrphanReport, error) {
	db := m.cfg.Diagnostics
	if db == nil {
		return nil, errors.New("the orphan collection needs the state DB")
	}
	dry := req.DryRun == nil || *req.DryRun
	max := req.Max
	if max <= 0 {
		max = orphanDefaultMax
	}
	if max > orphanMaxCap {
		max = orphanMaxCap
	}
	days := req.MinAgeDays
	if days <= 0 {
		days = orphanDefaultMinDays
	}
	if !m.diagMu.TryLock() {
		return nil, ErrDiagnosisRunning
	}
	defer m.diagMu.Unlock()
	start := time.Now()

	// A missing or empty media dir (an unmounted share) reads as "no stubs": every torrent
	// would look unreferenced.
	for _, dir := range []string{m.cfg.MoviesDir, m.cfg.TVDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, errf(http.StatusConflict, "media directory %s is not readable: refusing to collect", dir)
		}
	}
	// Read before the torrents: a counter recorded later belongs to a torrent the second
	// engine read already sees.
	failures, err := db.MetadataFailureHashes()
	if err != nil {
		return nil, fmt.Errorf("metadata failures: %w", err)
	}
	snap, err := m.snapshotRefs(ctx, db)
	if err != nil {
		return nil, err
	}
	if n := snap.unreadableStubs(); n > 0 {
		return nil, errf(http.StatusConflict, "%d stub(s) cannot be read or carry no hash: their torrent cannot be told, see stubs_without_hash in the consistency check", n)
	}
	movieStubs, tvStubs := 0, 0
	for _, s := range snap.stubs {
		if strings.HasPrefix(s.Path, m.cfg.TVDir+string(filepath.Separator)) {
			tvStubs++
		} else {
			movieStubs++
		}
	}
	if movieStubs == 0 || tvStubs == 0 {
		return nil, errf(http.StatusConflict, "no stubs under %s: refusing to collect", map[bool]string{true: m.cfg.MoviesDir, false: m.cfg.TVDir}[movieStubs == 0])
	}
	orphans := snap.orphans(start.Add(-time.Duration(days) * 24 * time.Hour))
	if len(snap.torrents) > 0 && len(orphans)*100 > len(snap.torrents)*orphanMaxSharePct {
		return nil, errf(http.StatusConflict, "%d of %d torrents look unreferenced, over %d%%: something is off, refusing to collect",
			len(orphans), len(snap.torrents), orphanMaxSharePct)
	}
	// A torrent being played, or hydrated in memory, is in use whatever the library says:
	// a direct link, or a sync re-using a stored torrent before its stub is written.
	active := map[string]bool{}
	if a, ok := m.cfg.GoStorm.(interface {
		ActiveHashes(ctx context.Context) ([]string, error)
	}); ok {
		hashes, err := a.ActiveHashes(ctx)
		if err != nil {
			return nil, fmt.Errorf("active torrents: %w", err)
		}
		for _, h := range hashes {
			active[diagHash(h)] = true
		}
	}
	kept := orphans[:0]
	for _, t := range orphans {
		h := diagHash(t.Hash)
		if active[h] || (m.cfg.ActiveSession != nil && m.cfg.ActiveSession(h)) {
			continue
		}
		kept = append(kept, t)
	}
	orphans = kept
	rep := &OrphanReport{DryRun: dry, Candidates: len(orphans), Selected: []Orphan{}, Released: []string{}, Kept: []string{}, Failed: []string{}, StaleFailures: []string{}}
	for _, f := range failures {
		// A referenced hash may be hydrated in memory only, its row not yet written.
		if h := diagHash(f); !snap.engine[h] && !snap.referenced[h] {
			rep.StaleFailures = append(rep.StaleFailures, f)
		}
	}
	if len(orphans) > max {
		orphans = orphans[:max]
	}
	for _, t := range orphans {
		title, _, _ := strings.Cut(t.Title, "\n")
		rep.Selected = append(rep.Selected, Orphan{Hash: diagHash(t.Hash), Title: title, Added: time.Unix(t.Timestamp, 0).Format("2006-01-02")})
	}
	if !dry {
		// A client hanging up must not cut the run short: each removal is logged, so the
		// record survives a report nobody reads.
		rctx := context.WithoutCancel(ctx)
		if req.CountersOnly {
			rep.Selected = []Orphan{}
		}
		for i, o := range rep.Selected {
			// A long run reads the library again every batch: a stub written meanwhile,
			// even one whose name does not match its hash, protects its torrent.
			if i > 0 && i%orphanBatch == 0 {
				fresh, err := m.snapshotRefs(rctx, db)
				if err != nil {
					rep.Failed = append(rep.Failed, fmt.Sprintf("stopped after %d: %v", i, err))
					break
				}
				snap = fresh
			}
			if snap.referenced[o.Hash] {
				rep.Kept = append(rep.Kept, o.Hash)
				m.cfg.Logger.Printf("[LibraryAPI] orphan %s (%s) kept: referenced on the re-read", o.Hash, o.Title)
				continue
			}
			released, err := m.ReleaseIfUnused(rctx, o.Hash)
			switch {
			case err != nil:
				rep.Failed = append(rep.Failed, fmt.Sprintf("%s: %v", o.Hash, err))
				m.cfg.Logger.Printf("[LibraryAPI] orphan %s (%s) not removed: %v", o.Hash, o.Title, err)
			case released:
				rep.Released = append(rep.Released, o.Hash)
				m.cfg.Logger.Printf("[LibraryAPI] orphan %s (%s, added %s) removed", o.Hash, o.Title, o.Added)
			default:
				rep.Kept = append(rep.Kept, o.Hash)
				m.cfg.Logger.Printf("[LibraryAPI] orphan %s (%s) kept: referenced on the re-check", o.Hash, o.Title)
			}
		}
		if cf, ok := m.cfg.Registry.(interface{ ClearMetadataFailure(string) error }); ok {
			for _, f := range rep.StaleFailures {
				if err := cf.ClearMetadataFailure(f); err != nil {
					rep.Failed = append(rep.Failed, fmt.Sprintf("failure counter %s: %v", f, err))
				}
			}
		}
		// The cached check now describes a library that changed: drop it rather than show it.
		if len(rep.Released) > 0 {
			m.lastDiag.Store(nil)
		}
		m.cfg.Logger.Printf("[LibraryAPI] orphan collection: %d candidate(s), %d released, %d kept, %d failed, %d stale failure counter(s) cleared",
			rep.Candidates, len(rep.Released), len(rep.Kept), len(rep.Failed), len(rep.StaleFailures))
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	return rep, nil
}

// AutoCollect runs one real collection with the configured limits, for the scheduler.
// The guards still apply: a refusal is logged and nothing is removed.
func (m *Manager) AutoCollect(ctx context.Context, minAgeDays, max int) {
	no := false
	rep, err := m.CollectOrphans(ctx, OrphanRequest{DryRun: &no, Max: max, MinAgeDays: minAgeDays})
	if err != nil {
		m.cfg.Logger.Printf("[LibraryAPI] orphan collection after the sync skipped: %v", err)
		return
	}
	_ = rep // CollectOrphans logs each removal and the summary
}
