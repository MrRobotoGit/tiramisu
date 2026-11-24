// Package friendlib connects the friends library protocol to the library on disk: a feed of the
// master's stubs and a sink that mirrors them on a slave.
package friendlib

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/friends"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
)

// FeedStore is the feed's persistence; *metadb.DB satisfies it.
type FeedStore interface {
	PutFeedEntry(path string, del bool, entry string) (int64, error)
	GetFeedEntry(path string) (metadb.FeedRow, bool, error)
	FeedScan(cursor int64, skipDeletes bool, visit func(metadb.FeedRow) bool) error
	FeedCount(live bool, afterSeq int64) (int64, error)
	FeedHead() (int64, error)
	PruneFeedDeletes(upTo int64) error
	ClearFeed() error
}

// FeedConfig wires a Feed.
type FeedConfig struct {
	Store    FeedStore
	Root     string // the media root holding movies/ and tv/
	Registry library.EpisodeRegistry
	HasInfo  func(hash string) bool   // the row has the metainfo
	Info     func(hash string) []byte // the metainfo, nil when absent
	Changed  func()                   // a new feed row: tell the slaves
	Logf     func(format string, args ...any)
}

// Feed is the master's library as its slaves pull it: one row per stub, read from disk.
type Feed struct {
	cfg      FeedConfig
	on       atomic.Bool // the node has slaves
	closed   atomic.Bool // the process is shutting down
	building atomic.Bool // the first build runs: pulls wait for its news
	mu       sync.Mutex  // one writer of the feed at a time
	skipped  map[string]bool

	recMu      sync.Mutex
	recRunning bool
	recAgain   bool

	hashMu sync.Mutex
	byPath map[string]string // live feed path -> hash: only these hashes' metainfo is served
	refs   map[string]int
}

// setLive records rel's live hash ("" when it has none).
func (f *Feed) setLive(rel, hash string) {
	f.hashMu.Lock()
	defer f.hashMu.Unlock()
	if old, ok := f.byPath[rel]; ok {
		if f.refs[old]--; f.refs[old] <= 0 {
			delete(f.refs, old)
		}
		delete(f.byPath, rel)
	}
	if hash != "" {
		f.byPath[rel] = hash
		f.refs[hash]++
	}
}

func (f *Feed) live(hash string) bool {
	f.hashMu.Lock()
	defer f.hashMu.Unlock()
	return f.refs[hash] > 0
}

var _ friends.LibrarySource = (*Feed)(nil)

func NewFeed(cfg FeedConfig) *Feed {
	if cfg.Changed == nil {
		cfg.Changed = func() {}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Feed{cfg: cfg, skipped: map[string]bool{}, byPath: map[string]string{}, refs: map[string]int{}}
}

func (f *Feed) logf(format string, args ...any) { f.cfg.Logf("[Friends] "+format, args...) }

// rel turns an absolute stub path into the feed's key; false outside movies/ and tv/.
func (f *Feed) rel(abs string) (string, bool) {
	r, err := filepath.Rel(f.cfg.Root, abs)
	if err != nil {
		return "", false
	}
	r = filepath.ToSlash(r)
	return r, (strings.HasPrefix(r, "movies/") || strings.HasPrefix(r, "tv/")) && friends.IsStubName(r)
}

// Touch re-reads one stub path and records what changed; nothing without slaves.
func (f *Feed) Touch(abs string) {
	if !f.on.Load() {
		return // the cheap answer without slaves; checked again under the lock
	}
	rel, ok := f.rel(abs)
	if !ok {
		return
	}
	f.mu.Lock()
	changed := f.on.Load() && f.touchLocked(rel)
	f.mu.Unlock()
	if changed {
		f.cfg.Changed()
	}
}

func (f *Feed) touchLocked(rel string) bool {
	cur, has, err := f.cfg.Store.GetFeedEntry(rel)
	if err != nil {
		f.logf("library feed %s: %v", rel, err)
		return false
	}
	st, present, err := library.StubEntry(f.cfg.Root, rel, f.cfg.Registry)
	if err != nil {
		f.logf("library feed %s: %v", rel, err)
		return false
	}
	var e friends.LibEntry
	if present {
		e = friends.LibEntry{Path: rel, Hash: st.Hash, Index: st.Index, Size: st.Size, IMDB: st.IMDB,
			Key: st.Key, Score: st.Score, Show: st.ShowIMDB}
		e.Info = e.Hash != "" && f.cfg.HasInfo(e.Hash)
		if !e.Valid() {
			if !f.skipped[rel] {
				f.skipped[rel] = true
				f.logf("library feed: %s not shared (no readable hash, file index or size)", rel)
			}
			present = false
		}
	}
	if present {
		f.setLive(rel, e.Hash)
	} else {
		f.setLive(rel, "")
	}
	if !present {
		if !has || cur.Del {
			return false
		}
		if err := json.Unmarshal([]byte(cur.Entry), &e); err != nil {
			e = friends.LibEntry{Path: rel}
		}
		e.Del = true
	}
	b, _ := json.Marshal(e)
	if has && cur.Del == e.Del && cur.Entry == string(b) {
		return false
	}
	if _, err := f.cfg.Store.PutFeedEntry(rel, e.Del, string(b)); err != nil {
		f.logf("library feed %s: %v", rel, err)
		return false
	}
	return true
}

// Reconcile walks every stub and repairs the rows a missed write or removal left wrong. Calls
// during a walk coalesce into one more walk after it.
func (f *Feed) Reconcile() {
	if !f.on.Load() {
		return
	}
	f.recMu.Lock()
	if f.recRunning {
		f.recAgain = true
		f.recMu.Unlock()
		return
	}
	f.recRunning = true
	f.recMu.Unlock()
	for {
		f.reconcileOnce()
		f.recMu.Lock()
		if !f.recAgain {
			f.recRunning = false
			f.recMu.Unlock()
			return
		}
		f.recAgain = false
		f.recMu.Unlock()
	}
}

func (f *Feed) reconcileOnce() {
	start := time.Now()
	seen := map[string]bool{}
	changes := 0
	stopped := false
	// One path at a time under the lock: writers' Touch never waits for the whole walk.
	touch := func(rel string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.on.Load() || f.closed.Load() {
			stopped = true
			return false
		}
		if f.touchLocked(rel) {
			changes++
		}
		return true
	}
	for _, dir := range []string{"movies", "tv"} {
		top := filepath.Join(f.cfg.Root, dir)
		_ = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if !os.IsNotExist(err) {
					f.logf("library feed: %s: %v", p, err) // one unreadable folder never hides the rest
				}
				if d != nil && d.IsDir() && p != top {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if rel, ok := f.rel(p); ok {
				seen[rel] = true
				if !touch(rel) {
					return filepath.SkipAll
				}
			}
			return nil
		})
		if stopped {
			return // stopped or shutting down: the next build repairs the rest
		}
	}
	var gone []string
	_ = f.cfg.Store.FeedScan(0, true, func(r metadb.FeedRow) bool {
		if !seen[r.Path] {
			gone = append(gone, r.Path)
		}
		return true
	})
	for _, rel := range gone {
		if !touch(rel) {
			return
		}
	}
	f.logf("library feed: %d stubs, %d changes in %v", len(seen), changes, time.Since(start).Round(time.Millisecond))
	if changes > 0 {
		f.cfg.Changed()
	}
}

// Since returns the rows after cursor that fit, the last seq they cover and how many remain.
func (f *Feed) Since(cursor int64, fit func([]friends.LibEntry) bool) ([]friends.LibEntry, int64, int64, error) {
	if f.building.Load() {
		return nil, cursor, -1, nil
	}
	skip := cursor == 0 // the first alignment needs no deletions
	// Read before the scan: a row added later has a seq above it and is sent next time.
	head, err := f.cfg.Store.FeedHead()
	if err != nil {
		return nil, cursor, 0, err
	}
	var out []friends.LibEntry
	last := cursor
	var scanErr error
	err = f.cfg.Store.FeedScan(cursor, skip, func(r metadb.FeedRow) bool {
		var e friends.LibEntry
		if err := json.Unmarshal([]byte(r.Entry), &e); err != nil {
			scanErr = err
			return false
		}
		e.Del = r.Del
		if fit(append(out, e)) {
			out = append(out, e)
			last = r.Seq
			return true
		}
		if len(out) == 0 { // larger than a frame on its own: never sendable
			f.logf("library feed: %s too large for a frame, skipped", r.Path)
			last = r.Seq
			return true
		}
		return false
	})
	if err == nil {
		err = scanErr
	}
	if err != nil {
		return nil, cursor, 0, err
	}
	left, err := f.cfg.Store.FeedCount(skip, last)
	if err != nil {
		return nil, cursor, 0, err
	}
	if left == 0 {
		last = max(last, head)
	}
	return out, last, left, nil
}

// Counts are the live entries up to cursor, all live entries, and the rows after cursor.
func (f *Feed) Counts(cursor int64) (entries, total, waiting int64, err error) {
	if total, err = f.cfg.Store.FeedCount(true, 0); err != nil {
		return
	}
	liveAfter, err := f.cfg.Store.FeedCount(true, cursor)
	if err != nil {
		return
	}
	waiting, err = f.cfg.Store.FeedCount(false, cursor)
	return total - liveAfter, total, waiting, err
}

// Info serves the metainfo of a hash the feed publishes, and of no other.
func (f *Feed) Info(hash string) ([]byte, error) {
	if !f.live(hash) {
		return nil, nil
	}
	return f.cfg.Info(hash), nil
}

func (f *Feed) Head() (int64, error) { return f.cfg.Store.FeedHead() }

func (f *Feed) Confirmed(minCursor int64) error { return f.cfg.Store.PruneFeedDeletes(minCursor) }

// Start builds the feed for the first slave.
func (f *Feed) Start() error {
	f.building.Store(true)
	defer f.building.Store(false)
	f.mu.Lock()
	f.on.Store(true)
	f.mu.Unlock()
	f.Reconcile()
	return nil
}

// Close stops a walk in progress and any later one (shutdown).
func (f *Feed) Close() { f.closed.Store(true) }

// Stop drops the feed: no slave left.
func (f *Feed) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on.Store(false)
	f.hashMu.Lock()
	clear(f.byPath)
	clear(f.refs)
	f.hashMu.Unlock()
	return f.cfg.Store.ClearFeed()
}
