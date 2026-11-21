package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/fileorder"
	"tiramisu/internal/piecestore"
)

// File is one file of a torrent; ID is the 1-based ID of the stubs (engine.md §1.6).
type File struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
	Offset int64  `json:"-"`
}

// Meta is what a caller attaches to a torrent.
type Meta struct {
	Title, Category, Poster, Data string
}

// Torrent is a hydrated torrent, or a DB-only view (State InDB, Hydrated false).
type Torrent struct {
	e    *Engine
	hash string
	bt   *bt.Torrent // nil when DB-only

	IsPriority atomic.Bool // set by Tiramisu; only effect: never expires

	// life serialises save, expiry, close and Remove of this wrapper; taken before addMu.
	life sync.Mutex

	store     atomic.Pointer[piecestore.TorrentStore]
	state     atomic.Int32
	deadline  atomic.Int64 // unix nanos
	consumers atomic.Int32

	mu       sync.Mutex
	meta     Meta
	added    int64 // timestamp column: added (new rows) or the stored value
	size     int64
	tiers    [][]string
	peers    []string // last known peer addresses (spec PeerAddrs)
	stored   bool     // a DB row with metainfo exists
	files    []File
	filesSet bool

	closed  chan struct{}
	working chan struct{} // closed on the move to Working

	stats torrentStats
	rates peerRates

	cold coldBoost
	aggr aggressive
}

func (t *Torrent) Hash() string { return t.hash }

// Hydrated reports whether t is a live torrent of the current client (not DB-only, not closed).
func (t *Torrent) Hydrated() bool { return t != nil && t.bt != nil && t.State() != StateClosed }

func (t *Torrent) State() State { return State(t.state.Load()) }

// transition moves from one state to another only if t is still in from.
func (t *Torrent) transition(from, to State) bool {
	return t.state.CompareAndSwap(int32(from), int32(to))
}

// toClosed enters Closed from any state; false when t was already closed.
func (t *Torrent) toClosed() bool {
	for {
		s := t.state.Load()
		if State(s) == StateClosed {
			return false
		}
		if t.state.CompareAndSwap(s, int32(StateClosed)) {
			return true
		}
	}
}

// Closed is closed when the torrent leaves the registry.
func (t *Torrent) Closed() <-chan struct{} { return t.closed }

// Store is the torrent's piece store, nil before metadata.
func (t *Torrent) Store() *piecestore.TorrentStore { return t.store.Load() }

func (t *Torrent) setStore(ts *piecestore.TorrentStore) { t.store.Store(ts) }

func (t *Torrent) Meta() Meta {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta
}

func (t *Torrent) Title() string { return t.Meta().Title }

// Timestamp is the "added" time in unix seconds.
func (t *Torrent) Timestamp() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.added
}

// Size is the torrent length, or the stored size before metadata.
func (t *Torrent) Size() int64 {
	if t.bt != nil {
		if n := t.bt.Length(); n > 0 {
			return n
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size
}

// Deadline is the expiry deadline.
func (t *Torrent) Deadline() time.Time { return time.Unix(0, t.deadline.Load()) }

// extend moves the deadline to at least now+d; it never moves it earlier.
func (t *Torrent) extend(d time.Duration) {
	next := t.e.now().Add(d).UnixNano()
	for {
		cur := t.deadline.Load()
		if next <= cur || t.deadline.CompareAndSwap(cur, next) {
			return
		}
	}
}

// AddExpiredTime extends the expiry by d (main gives 30s of grace on playback stop).
func (t *Torrent) AddExpiredTime(d time.Duration) {
	if t.bt != nil {
		t.extend(d)
	}
}

// Acquire registers a consumer (a stream or a demand read): t never expires while one is
// held. The release of the last one extends the expiry by TorrentDisconnectTimeout. ok is
// false, with a no-op release, on a closed or DB-only torrent: look it up again.
func (t *Torrent) Acquire() (release func(), ok bool) {
	if t.bt == nil {
		return func() {}, false
	}
	t.life.Lock() // expiry checks consumers under the same lock
	defer t.life.Unlock()
	if t.State() == StateClosed {
		return func() {}, false
	}
	t.consumers.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			if t.consumers.Add(-1) == 0 {
				t.extend(t.e.config().timeout())
			}
		})
	}, true
}

// Consumers is the number of held consumers.
func (t *Torrent) Consumers() int { return int(t.consumers.Load()) }

// Files lists the files in ID order; nil before metadata and for DB-only torrents.
func (t *Torrent) Files() []File {
	if t.bt == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.filesSet {
		engineOrder := t.bt.Files()
		if engineOrder == nil {
			return nil
		}
		t.files = assignIDs(engineOrder)
		t.filesSet = true
	}
	return append([]File(nil), t.files...)
}

// EngineFileIndex maps a file ID to the library's file index (the fork's warmup hooks take
// it); -1 when unknown.
func (t *Torrent) EngineFileIndex(id int) int {
	files := t.Files()
	if id < 1 || id > len(files) {
		return -1
	}
	want := files[id-1]
	for i, f := range t.bt.Files() {
		if f.Offset == want.Offset && f.Path == want.Path && f.Length == want.Length {
			return i
		}
	}
	return -1
}

// assignIDs sorts a copy of the engine order with fileorder.Sort and numbers it from 1.
func assignIDs(engineOrder []bt.File) []File {
	paths := make([]string, len(engineOrder))
	byPath := map[string][]bt.File{}
	for i, f := range engineOrder {
		paths[i] = f.Path
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	out := make([]File, len(paths))
	for i, p := range fileorder.Sort(paths) {
		f := byPath[p][0]
		byPath[p] = byPath[p][1:]
		out[i] = File{ID: i + 1, Path: f.Path, Length: f.Length, Offset: f.Offset}
	}
	return out
}

// fillMeta sets each empty field from the first non-empty candidate.
func (t *Torrent) fillMeta(cands ...Meta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range cands {
		if t.meta.Title == "" {
			t.meta.Title = c.Title
		}
		if t.meta.Category == "" {
			t.meta.Category = c.Category
		}
		if t.meta.Poster == "" {
			t.meta.Poster = c.Poster
		}
		if t.meta.Data == "" {
			t.meta.Data = c.Data
		}
	}
}

// PiecesEvicted tells the library that evicted pieces are gone, so it fetches them again.
// The eviction loop (plan 3) calls it after Evict, holding no store lock.
func (t *Torrent) PiecesEvicted(pieces []int) {
	if t.bt == nil {
		return
	}
	for _, p := range pieces {
		t.bt.UpdateCompletion(p)
	}
}
