package piecestore

import (
	"log"
	"sync"
	"time"
)

// Config of a Store.
type Config struct {
	Capacity  int64                              // per torrent, bytes (CacheSize)
	ChunkSize int                                // default DefaultChunkSize
	Settings  func() (responsive, adaptive bool) // user ResponsiveMode and AdaptiveShield
	Logf      func(format string, args ...any)
	OnCorrupt func(hash string, off, n int64) // a piece failed its hash (TorrentStore.onCorrupt)
}

// Store maps infohash (lowercase hex) to TorrentStore.
type Store struct {
	cfg Config

	mu       sync.Mutex
	torrents map[string]*TorrentStore
	pools    map[int]*ChunkPool // by chunk size
}

func New(cfg Config) *Store {
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	if cfg.Settings == nil {
		cfg.Settings = func() (bool, bool) { return true, true }
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.ChunkSize <= 0 || cfg.ChunkSize%unitSize != 0 {
		panic("piecestore: chunk size must be a positive multiple of 1KiB")
	}
	return &Store{cfg: cfg, torrents: map[string]*TorrentStore{}, pools: map[int]*ChunkPool{}}
}

func (s *Store) Capacity() int64 { return s.cfg.Capacity }

// chunkSize keeps pieces smaller than a chunk from holding a whole chunk each.
func (s *Store) chunkSize(pieceLen int64) int {
	if pieceLen > 0 && pieceLen%unitSize == 0 {
		return int(min(int64(s.cfg.ChunkSize), pieceLen))
	}
	return s.cfg.ChunkSize
}

func (s *Store) poolLocked(size int) *ChunkPool {
	p := s.pools[size]
	if p == nil {
		p = NewChunkPool(size, s.cfg.Capacity/4)
		s.pools[size] = p
	}
	return p
}

// Open creates the store of a torrent, replacing (not closing) a previous one for the hash:
// the engine closes the old instance through Remove.
func (s *Store) Open(hash string, pieceLen, length int64) *TorrentStore {
	sh := newShield(hash, shieldConfig{enabled: s.cfg.Settings, logf: s.cfg.Logf, now: time.Now, poll: time.Second})
	s.mu.Lock()
	t := newTorrentStore(hash, pieceLen, length, s.cfg.Capacity, s.poolLocked(s.chunkSize(pieceLen)), sh)
	t.onCorrupt = s.cfg.OnCorrupt
	s.torrents[hash] = t
	s.mu.Unlock()
	return t
}

// Get returns the open store for hash, or nil (also after Remove).
func (s *Store) Get(hash string) *TorrentStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.torrents[hash]
}

// Remove closes t and forgets it only if it is still the current store for its hash: the
// only way to close a store, by instance, so a stale close never hits a re-added torrent.
func (s *Store) Remove(t *TorrentStore) {
	s.mu.Lock()
	if s.torrents[t.hash] == t {
		delete(s.torrents, t.hash)
	}
	s.mu.Unlock()
	t.Close()
}

func (s *Store) all() []*TorrentStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*TorrentStore, 0, len(s.torrents))
	for _, t := range s.torrents {
		out = append(out, t)
	}
	return out
}

// ShieldStatus aggregates every torrent for /metrics: active if any is, the latest
// corruption, the largest clean streak required.
func (s *Store) ShieldStatus() (active bool, last int64, need int64) {
	need = requiredStreak(0)
	for _, t := range s.all() {
		a, l, n := t.shield.Status()
		active = active || a
		last = max(last, l)
		need = max(need, n)
	}
	return active, last, need
}

// ResetShields resets every torrent's shield.
func (s *Store) ResetShields() {
	for _, t := range s.all() {
		t.shield.Reset()
	}
}
