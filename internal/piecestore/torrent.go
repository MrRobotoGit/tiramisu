package piecestore

import (
	"sync"
	"sync/atomic"
	"time"
)

// TorrentStore holds the pieces of one infohash.
type TorrentStore struct {
	hash     string
	pieceLen int64
	length   int64
	capacity int64
	pool     *ChunkPool
	shield   *Shield
	pieces   []*Piece

	filled atomic.Int64
	closed atomic.Bool
	once   sync.Once
	done   chan struct{}
	onData atomic.Pointer[func()]
	// lastData: unix nanos of the last write or verification into any piece (0: none)
	lastData atomic.Int64
	received atomic.Int64 // bytes written into pieces
	// onCorrupt gets the torrent byte range of a piece that failed its hash: copies of its
	// unverified bytes kept outside the store (read-ahead cache, SSD warmup) are wrong.
	onCorrupt func(hash string, off, n int64)
}

func newTorrentStore(hash string, pieceLen, length, capacity int64, pool *ChunkPool, shield *Shield) *TorrentStore {
	t := &TorrentStore{hash: hash, pieceLen: pieceLen, length: length, capacity: capacity, pool: pool, shield: shield, done: make(chan struct{})}
	if pieceLen > 0 && length > 0 {
		n := (length + pieceLen - 1) / pieceLen
		t.pieces = make([]*Piece, n)
		for i := range t.pieces {
			t.pieces[i] = &Piece{index: i, length: min(pieceLen, length-int64(i)*pieceLen), owner: t}
		}
	}
	return t
}

func (t *TorrentStore) Hash() string     { return t.hash }
func (t *TorrentStore) PieceLen() int64  { return t.pieceLen }
func (t *TorrentStore) Length() int64    { return t.length }
func (t *TorrentStore) Capacity() int64  { return t.capacity }
func (t *TorrentStore) NumPieces() int   { return len(t.pieces) }
func (t *TorrentStore) Shield() *Shield  { return t.shield }
func (t *TorrentStore) Pool() *ChunkPool { return t.pool }
func (t *TorrentStore) Filled() int64    { return t.filled.Load() }
func (t *TorrentStore) Closed() bool     { return t.closed.Load() }
func (t *TorrentStore) Responsive() bool { return t.shield.Responsive() }

// LastData is when any piece last received data or was verified (zero time: never).
func (t *TorrentStore) LastData() time.Time {
	if n := t.lastData.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (t *TorrentStore) touchData() { t.lastData.Store(time.Now().UnixNano()) }

// Received is the number of bytes written into the store's pieces since it opened.
func (t *TorrentStore) Received() int64 { return t.received.Load() }

// Done is closed when the store closes.
func (t *TorrentStore) Done() <-chan struct{} { return t.done }

// Piece returns piece i, or nil when out of range. Objects are permanent for the store's life.
func (t *TorrentStore) Piece(i int) *Piece {
	if i < 0 || i >= len(t.pieces) {
		return nil
	}
	return t.pieces[i]
}

// SetOnData installs the hook called when an empty piece receives its first data.
func (t *TorrentStore) SetOnData(f func()) {
	if f == nil {
		t.onData.Store(nil)
		return
	}
	t.onData.Store(&f)
}

func (t *TorrentStore) dataArrived() {
	if f := t.onData.Load(); f != nil {
		(*f)()
	}
}

// MarkNotComplete is the engine's "hash failed / not complete" for piece i.
func (t *TorrentStore) MarkNotComplete(i int) {
	if p := t.Piece(i); p != nil && p.markNotComplete() && !t.Closed() {
		t.shield.Corruption(i)
		if f := t.onCorrupt; f != nil {
			go f(t.hash, int64(i)*t.pieceLen, p.length) // the engine calls this under its lock
		}
	}
}

// PieceInfo is the per-piece input of the eviction policy.
type PieceInfo struct {
	Index     int
	Allocated int64
	State     State
	Accessed  int64
	Writes    uint64
}

// Snapshot lists the pieces holding memory, in index order.
func (t *TorrentStore) Snapshot() []PieceInfo {
	var out []PieceInfo
	for _, p := range t.pieces {
		if a := p.Allocated(); a > 0 {
			out = append(out, PieceInfo{Index: p.index, Allocated: a, State: p.State(), Accessed: p.Accessed(), Writes: p.WriteCount()})
		}
	}
	return out
}

// Evict frees the selected pieces that are still verified and unwritten since the snapshot.
func (t *TorrentStore) Evict(sel []PieceInfo) (freed int64, evicted []int) {
	if t.Closed() {
		return 0, nil
	}
	for _, e := range sel {
		if p := t.Piece(e.Index); p != nil {
			if n, ok := p.releaseIfVerified(e.Writes); ok {
				freed += n
				evicted = append(evicted, e.Index)
			}
		}
	}
	return freed, evicted
}

// Close drops every buffer (not recycled) and wakes every waiter. Idempotent.
func (t *TorrentStore) Close() {
	t.once.Do(func() {
		t.closed.Store(true)
		for _, p := range t.pieces {
			p.drop()
		}
		t.shield.Close()
		close(t.done)
	})
}
