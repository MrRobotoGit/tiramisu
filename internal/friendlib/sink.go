package friendlib

import (
	"context"
	"encoding/json"
	"path"
	"sync"
	"time"

	"tiramisu/internal/friends"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
)

// SinkStore is the mirror's persistence; *metadb.DB satisfies it.
type SinkStore interface {
	PutMirror(metadb.MirrorRow) error
	GetMirror(path string) (metadb.MirrorRow, bool, error)
	ListMirrorState(states ...string) ([]metadb.MirrorRow, error)
	PruneMirrorRemoved(beforeSess int64, keep ...string) error
}

// SinkConfig wires a Sink.
type SinkConfig struct {
	Store    SinkStore
	Library  *library.Manager
	Register func(hash string, info []byte, title string, trackers [][]string) error
	HasInfo  func(hash string) bool
	Logf     func(format string, args ...any)
}

// Sink applies a master's entries on a slave through the library.
type Sink struct {
	store   SinkStore
	cfg     SinkConfig
	pending sync.Mutex // one ApplyPending at a time

	pathMu sync.Mutex
	paths  map[string]*pathLock // per path: a removal and a late pending apply never interleave

	inflight map[string]int // paths a pending apply is working on: their rows are never pruned (pathMu)

	retryMu sync.Mutex
	retry   map[string]retryAt // failed paths: next attempt, with the friends backoff
	now     func() time.Time
}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

type retryAt struct {
	n  int
	at time.Time
}

// failedAgain pushes path's next retry back; done forgets it.
func (s *Sink) failedAgain(path string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	r := s.retry[path]
	r.n++
	r.at = s.now().Add(friends.RetryDelay(r.n))
	s.retry[path] = r
}

func (s *Sink) done(path string) {
	s.retryMu.Lock()
	delete(s.retry, path)
	s.retryMu.Unlock()
}

func (s *Sink) due(path string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	r, ok := s.retry[path]
	return !ok || !r.at.After(s.now())
}

// lockPath serialises the decisions on one path; never held across a swarm wait.
func (s *Sink) lockPath(p string) func() {
	s.pathMu.Lock()
	l := s.paths[p]
	if l == nil {
		l = &pathLock{}
		s.paths[p] = l
	}
	l.refs++
	s.pathMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.pathMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.paths, p)
		}
		s.pathMu.Unlock()
	}
}

var _ friends.LibrarySink = (*Sink)(nil)

func NewSink(cfg SinkConfig) *Sink {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Sink{store: cfg.Store, cfg: cfg, paths: map[string]*pathLock{}, inflight: map[string]int{}, retry: map[string]retryAt{}, now: time.Now}
}

func (s *Sink) logf(format string, args ...any) { s.cfg.Logf("[Friends] "+format, args...) }

// record writes what happened to e; only this write failing is a node error.
func (s *Sink) record(e friends.LibEntry, state, reason string, sess int64) error {
	if state == "failed" {
		s.failedAgain(e.Path)
	} else {
		s.done(e.Path)
	}
	b, _ := json.Marshal(e)
	now := s.now()
	return s.store.PutMirror(metadb.MirrorRow{Path: e.Path, Seq: now.UnixNano(), State: state, Entry: string(b),
		Reason: reason, At: now.Unix(), Sess: sess})
}

func mirrorEntry(e friends.LibEntry, hasInfo bool) library.MirrorEntry {
	return library.MirrorEntry{Path: e.Path, Hash: e.Hash, Index: e.Index, Size: e.Size, IMDB: e.IMDB, Key: e.Key,
		Score: e.Score, ShowIMDB: e.Show, HasInfo: hasInfo}
}

func trackerTiers() [][]string {
	var out [][]string
	for _, t := range library.DefaultTrackers() {
		out = append(out, []string{t})
	}
	return out
}

// Apply registers the metainfo (when given) and writes the stub; an entry failure is recorded.
func (s *Sink) Apply(ctx context.Context, e friends.LibEntry, info []byte, sess int64) error {
	defer s.lockPath(e.Path)() // no swarm wait here: the metainfo is registered
	if ctx.Err() != nil {
		return nil // the role ended: nothing is applied for that master any more
	}
	registered := false
	if info != nil && !s.cfg.HasInfo(e.Hash) {
		base := path.Base(e.Path)
		title := base[:len(base)-len(path.Ext(base))] // .mkv in any case
		if err := s.cfg.Register(e.Hash, info, title, trackerTiers()); err != nil {
			return s.Fail(e, "register: "+err.Error(), sess)
		}
		registered = true
	}
	if err := s.cfg.Library.Mirror(ctx, mirrorEntry(e, true)); err != nil {
		if registered { // a row no stub uses: gone again unless something else holds it
			if _, rerr := s.cfg.Library.ReleaseIfUnused(ctx, e.Hash); rerr != nil {
				s.logf("library: torrent %s not released: %v", e.Hash, rerr)
			}
		}
		return s.Fail(e, err.Error(), sess)
	}
	return s.record(e, "present", "", sess)
}

// Remove removes a stub the master removed, only if it came from the master.
func (s *Sink) Remove(ctx context.Context, e friends.LibEntry, sess int64) error {
	defer s.lockPath(e.Path)()
	if ctx.Err() != nil {
		return nil
	}
	r, ok, err := s.store.GetMirror(e.Path)
	if err != nil {
		return err
	}
	if !ok || r.State == "removed" {
		return nil
	}
	// A failed or pending row may still have its stub on disk (a failed removal, a failed registration).
	if r.State == "present" || s.cfg.Library.MirrorOnDisk(mirrorEntry(e, false)) {
		if err := s.cfg.Library.Unmirror(ctx, e.Path); err != nil {
			s.logf("library: cannot remove %s: %v", e.Path, err)
			return s.record(e, "failed", "remove: "+err.Error(), sess)
		}
	}
	return s.record(e, "removed", "", sess)
}

func (s *Sink) Defer(e friends.LibEntry, sess int64) error { return s.record(e, "pending", "", sess) }

func (s *Sink) Fail(e friends.LibEntry, reason string, sess int64) error {
	s.logf("library: %s not added: %s", e.Path, reason)
	return s.record(e, "failed", reason, sess)
}

// Prune drops confirmed removals, except those a pending apply in flight must still see.
func (s *Sink) Prune(sess int64) error {
	s.pathMu.Lock()
	defer s.pathMu.Unlock() // held: no apply starts or settles while the rows go
	keep := make([]string, 0, len(s.inflight))
	for p := range s.inflight {
		keep = append(keep, p)
	}
	return s.store.PruneMirrorRemoved(sess, keep...)
}

// Forget drops the retry schedule: the mirror it was about is gone.
func (s *Sink) Forget() {
	s.retryMu.Lock()
	clear(s.retry)
	s.retryMu.Unlock()
}

func (s *Sink) setInflight(path string, on bool) {
	s.pathMu.Lock()
	defer s.pathMu.Unlock()
	if on {
		s.inflight[path]++
	} else if s.inflight[path]--; s.inflight[path] <= 0 {
		delete(s.inflight, path)
	}
}

func (s *Sink) HasInfo(hash string) bool { return s.cfg.HasInfo(hash) }

// ApplyPending applies the pending and failed entries one at a time, waiting for the swarm if needed.
func (s *Sink) ApplyPending(ctx context.Context) error {
	if !s.pending.TryLock() {
		return nil
	}
	defer s.pending.Unlock()
	rows, err := s.store.ListMirrorState("pending", "failed")
	if err != nil {
		return err
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var e friends.LibEntry
		if err := json.Unmarshal([]byte(r.Entry), &e); err != nil || !e.Valid() || !s.due(r.Path) {
			continue // a failure waits for its backoff: a dead swarm is not asked every session
		}
		if e.Del { // a removal that failed: removals are always mirrored
			if err := s.retryRemoval(ctx, e, r); err != nil {
				return err
			}
			continue
		}
		s.setInflight(e.Path, true)
		merr := s.cfg.Library.Mirror(ctx, mirrorEntry(e, s.cfg.HasInfo(e.Hash)))
		unlock := s.lockPath(e.Path)
		err := s.settlePending(ctx, e, r, merr)
		unlock()
		s.setInflight(e.Path, false)
		if err != nil {
			return err
		}
	}
	return nil
}

// settlePending records a late apply; a session that handled the path meanwhile wins, and a stub
// the master removed during the wait is taken away again.
func (s *Sink) settlePending(ctx context.Context, e friends.LibEntry, r metadb.MirrorRow, merr error) error {
	if ctx.Err() != nil {
		return nil // the role ended during the wait: its rows are gone and stay gone
	}
	cur, ok, err := s.store.GetMirror(e.Path)
	if err != nil {
		return err
	}
	if !ok || cur.Seq != r.Seq {
		// A missing row is a leave (removals in flight are never pruned): the title stays.
		if merr == nil && ok && cur.State == "removed" {
			if err := s.cfg.Library.Unmirror(ctx, e.Path); err != nil {
				s.logf("library: cannot remove %s: %v", e.Path, err)
				return s.record(asDel(e), "failed", "remove: "+err.Error(), cur.Sess)
			}
		}
		return nil
	}
	if merr != nil {
		s.logf("library: %s still not added: %v", e.Path, merr)
		return s.record(e, "failed", merr.Error(), r.Sess)
	}
	return s.record(e, "present", "", r.Sess)
}

// retryRemoval runs a failed removal again, unless a session handled the path meanwhile.
func (s *Sink) retryRemoval(ctx context.Context, e friends.LibEntry, r metadb.MirrorRow) error {
	defer s.lockPath(e.Path)()
	if ctx.Err() != nil {
		return nil
	}
	if cur, ok, err := s.store.GetMirror(e.Path); err != nil || !ok || cur.Seq != r.Seq {
		return err
	}
	if err := s.cfg.Library.Unmirror(ctx, e.Path); err != nil {
		s.logf("library: still cannot remove %s: %v", e.Path, err)
		return s.record(e, "failed", "remove: "+err.Error(), r.Sess)
	}
	return s.record(e, "removed", "", r.Sess)
}

func asDel(e friends.LibEntry) friends.LibEntry { e.Del = true; return e }
