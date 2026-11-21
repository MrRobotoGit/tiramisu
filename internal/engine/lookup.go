package engine

import (
	"sort"
	"strings"

	"tiramisu/internal/metadb"
)

func (e *Engine) lookup(hash string) *Torrent {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.torrents[hash]
}

// Get returns the hydrated torrent for hash, or nil. It never reads the DB and never extends
// the expiry.
func (e *Engine) Get(hash string) *Torrent {
	if e == nil {
		return nil
	}
	h, err := ParseHash(hash)
	if err != nil {
		return nil
	}
	return e.lookup(h)
}

// Peek returns the hydrated torrent, else a DB-only view, else nil; nil also for a malformed
// hash or an engine not up. It never reactivates and never extends the expiry.
func (e *Engine) Peek(hash string) *Torrent {
	if e == nil {
		return nil
	}
	h, err := ParseHash(hash)
	if err != nil || !e.Up() {
		return nil
	}
	if t := e.lookup(h); t != nil {
		return t
	}
	r := e.row(h)
	if r == nil {
		return nil
	}
	t := &Torrent{e: e, hash: h, closed: make(chan struct{}), added: r.Timestamp, size: r.Size, stored: len(r.InfoBytes) > 0,
		meta: Meta{Title: r.Title, Category: r.Category, Poster: r.Poster, Data: r.Data}}
	t.state.Store(int32(StateInDB))
	return t
}

// ListHydrated lists the hydrated torrents, newest timestamp first, then title descending.
func (e *Engine) ListHydrated() []*Torrent {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	out := make([]*Torrent, 0, len(e.torrents))
	for _, t := range e.torrents {
		out = append(out, t)
	}
	e.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Timestamp(), out[j].Timestamp()
		if a != b {
			return a > b
		}
		return out[i].Title() > out[j].Title()
	})
	return out
}

// Ref is a torrent's hash and title.
type Ref struct {
	Hash  string `json:"hash"`
	Title string `json:"title"`
}

// ListRefs lists every stored torrent plus the hydrated ones not stored yet.
func (e *Engine) ListRefs() ([]Ref, error) {
	var rows []metadb.TorrentRef
	if e.deps.DB != nil {
		var err error
		if rows, err = e.deps.DB.ListGoStormTorrentRefs(); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool, len(rows))
	out := make([]Ref, 0, len(rows))
	for _, r := range rows {
		h := strings.ToLower(r.Hash)
		seen[h] = true
		out = append(out, Ref{Hash: h, Title: r.Title})
	}
	for _, t := range e.ListHydrated() {
		if !seen[t.hash] {
			out = append(out, Ref{Hash: t.hash, Title: t.Title()})
		}
	}
	return out, nil
}

// close removes t from the registry and drops its library torrent; the store closes through
// the library (TorrentImpl.Close), never directly.
func (e *Engine) close(t *Torrent) {
	t.life.Lock()
	defer t.life.Unlock()
	e.closeLocked(t)
}

// closeLocked is close with t.life held.
func (e *Engine) closeLocked(t *Torrent) {
	if !t.toClosed() {
		return
	}
	e.addMu.Lock()
	defer e.addMu.Unlock()
	e.mu.Lock()
	if e.torrents[t.hash] == t {
		delete(e.torrents, t.hash)
	}
	e.mu.Unlock()
	close(t.closed)
	if t.bt != nil {
		t.bt.Drop()
	}
}

// live reports whether t is still the registered, open wrapper of its hash.
func (e *Engine) live(t *Torrent) bool {
	return t.State() != StateClosed && e.lookup(t.hash) == t
}

// Drop closes a hydrated torrent; its DB row stays. Reports whether one was hydrated.
func (e *Engine) Drop(hash string) bool {
	t := e.Get(hash)
	if t == nil {
		return false
	}
	e.close(t)
	return true
}

// Remove closes the torrent if hydrated, then deletes its row and its metadata_failures
// counter in one transaction. An error means the row may still be there.
func (e *Engine) Remove(hash string) error {
	h, err := ParseHash(hash)
	if err != nil {
		return err
	}
	if t := e.lookup(h); t != nil {
		// Held until the row is gone: a save of t cannot write it back.
		t.life.Lock()
		defer t.life.Unlock()
		e.closeLocked(t)
	}
	if e.deps.DB == nil {
		return nil
	}
	err = e.deps.DB.DeleteGoStormTorrent(h)
	e.rows.invalidate(h)
	return err
}
