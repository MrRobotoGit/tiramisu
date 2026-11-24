package engine

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/warmup"
)

// served is a friends-only torrent serving one friend from the SSD warmup.
type served struct {
	friend, hash string
	t            atomic.Pointer[bt.Torrent]
	slot         atomic.Pointer[func()] // the background slot's release, until handed over or released
}

// takeSlot hands the held slot's release to the caller, at most once.
func (s *served) takeSlot() func() {
	if p := s.slot.Swap(nil); p != nil {
		return *p
	}
	return nil
}

func (s *served) releaseSlot() {
	if f := s.takeSlot(); f != nil {
		f()
	}
}

type friendServe struct {
	warm     atomic.Pointer[warmup.DiskWarmupCache]
	mu       sync.Mutex
	byFriend map[string]*served // one served torrent per friend
	verdicts bt.VerdictCache    // pieces verified good, per warm file
}

// ServeWarmup lets friends read the SSD warmup; nil disables it. The warmup index is built at the
// first friend's use, so installs without friends pay nothing.
func (e *Engine) ServeWarmup(d *warmup.DiskWarmupCache) {
	if e == nil || e.friends == nil {
		return
	}
	e.serve.warm.Store(d)
	if d == nil {
		e.friends.SetWarm(nil, nil)
		e.dropServed("")
		return
	}
	e.friends.SetWarm(e.openWarm, func(f func([]byte) bool) bool {
		d.ServeIndex()
		return d.EachHash(f)
	})
}

// servedHashes lists the hashes being served (tests and logs).
func (e *Engine) servedHashes() []string {
	e.serve.mu.Lock()
	defer e.serve.mu.Unlock()
	var out []string
	for _, s := range e.serve.byFriend {
		out = append(out, s.hash)
	}
	return out
}

// openWarm loads a friends-only torrent over the warmup of ih for friend; called by the fork for a
// friend's candidate connection, which waits for it.
func (e *Engine) openWarm(friend string, ih [20]byte) bool {
	hash := hex.EncodeToString(ih[:])
	d := e.serve.warm.Load()
	if d == nil || e.config().DisableUpload || e.Get(hash) != nil || e.deps.DB == nil {
		return false
	}
	row, err := e.deps.DB.GetGoStormTorrent(hash)
	if err != nil || row == nil || len(row.InfoBytes) == 0 {
		return false
	}
	files, pieceLen, _, err := bt.InfoFiles(row.InfoBytes)
	if err != nil {
		return false
	}
	d.ServeIndex()
	byID := assignIDs(files)
	var length int64
	for _, f := range files {
		length += f.Length
	}
	fileLen := func(id int) int64 {
		if id < 1 || id > len(byID) {
			return 0
		}
		return byID[id-1].Length
	}
	var spans []bt.WarmSpan
	for _, sp := range d.Spans(hash, fileLen) {
		if sp.FileID < 1 || sp.FileID > len(byID) {
			continue
		}
		f := byID[sp.FileID-1]
		spans = append(spans, bt.WarmSpan{Off: f.Offset + sp.Start, Len: sp.End - sp.Start, FileOff: sp.PathOff, Path: sp.Path, Key: sp})
	}
	n := bt.WholePieces(spans, pieceLen, length)
	if n == 0 {
		return false
	}

	e.addMu.Lock() // lock order: addMu, serve.mu, client
	defer e.addMu.Unlock()
	c := e.currentClient()
	if c == nil || e.lookup(hash) != nil || e.config().DisableUpload {
		return false
	}
	var slot func() // the background slot, as a scan's pump
	if old := e.servedBy(friend); old != nil {
		if old.hash == hash {
			return true
		}
		if ot := old.t.Load(); ot != nil {
			if ot.Interested() {
				return false
			}
			slot = old.takeSlot() // handed over before the drop: the friend never loses it
			ot.Drop()
		}
	}
	if slot == nil {
		rel, ok := e.takeBackgroundSlot() // behind the owner's playback and scans: refused when busy
		if !ok {
			return false
		}
		slot = rel
	}
	s := &served{friend: friend, hash: hash}
	s.slot.Store(&slot)
	t, err := c.AddFriendsOnly(hash, row.InfoBytes, spans, func(key any) {
		if sp, ok := key.(warmup.Span); ok {
			e.servedBad(s, d, sp)
		}
	}, &e.serve.verdicts)
	if err != nil {
		s.releaseSlot()
		return false
	}
	s.t.Store(t)
	e.serve.mu.Lock()
	if e.serve.byFriend == nil {
		e.serve.byFriend = map[string]*served{}
	}
	e.serve.byFriend[friend] = s
	e.serve.mu.Unlock()
	go func() {
		<-t.Closed()
		e.serve.mu.Lock()
		if e.serve.byFriend[friend] == s {
			delete(e.serve.byFriend, friend)
		}
		e.serve.mu.Unlock()
		s.releaseSlot()
	}()
	e.deps.logf("[Friends] serving %s to %s: %d warm pieces", hash8(hash), friend, n)
	return true
}

func (e *Engine) servedBy(friend string) *served {
	e.serve.mu.Lock()
	defer e.serve.mu.Unlock()
	return e.serve.byFriend[friend]
}

// servedBad deletes a warm file whose piece failed its hash, only while its served torrent is
// open: a closed one may sit on a file a new hydration is rewriting. Under addMu, as hydrations
// close the served torrent before they start.
func (e *Engine) servedBad(s *served, d *warmup.DiskWarmupCache, sp warmup.Span) {
	e.addMu.Lock()
	defer e.addMu.Unlock()
	t := s.t.Load()
	if t == nil {
		return
	}
	select {
	case <-t.Closed():
		return
	default:
	}
	d.Invalidate(s.hash, sp.FileID, sp.Kind)
	e.deps.logf("[Friends] warm piece of %s failed its hash: %s deleted", hash8(s.hash), filepath.Base(sp.Path))
}

// dropServed closes the served torrents of hash ("" = all).
func (e *Engine) dropServed(hash string) {
	e.serve.mu.Lock()
	var drop []*bt.Torrent
	for _, s := range e.serve.byFriend {
		if t := s.t.Load(); t != nil && (hash == "" || s.hash == hash) {
			drop = append(drop, t)
		}
	}
	e.serve.mu.Unlock()
	for _, t := range drop {
		t.Drop()
	}
}

// addHydrating adds spec through c, closing a friends-only torrent that holds its hash first.
func addHydrating(c *bt.Client, spec bt.Spec, add func() (*bt.Torrent, bool, error)) (*bt.Torrent, bool, error) {
	lt, isNew, err := add()
	if errors.Is(err, bt.ErrFriendsOnlyHeld) {
		c.DropFriendsOnly(spec.InfoHash)
		lt, isNew, err = add()
	}
	return lt, isNew, err
}
