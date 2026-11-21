package piecestore

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// State of a piece's data.
type State int32

const (
	Empty    State = iota // no memory
	Partial               // some units written (or data failed the hash)
	Written               // every unit written, not verified
	Verified              // passed the hash check
)

var (
	ErrNotReady   = errors.New("piecestore: data not available")
	ErrOutOfRange = errors.New("piecestore: offset out of range")
	ErrClosed     = errors.New("piecestore: closed")
)

var nowUnix = func() int64 { return time.Now().Unix() }

var closedCh = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// Piece holds one piece's data in chunks from the store pool.
type Piece struct {
	index  int
	length int64
	owner  *TorrentStore

	mu     sync.RWMutex
	chunks [][]byte // nil until the first write
	units  unitSet

	state    atomic.Int32
	writes   atomic.Uint64
	accessed atomic.Int64
	alloc    atomic.Int64

	sigMu sync.Mutex
	sig   chan struct{}
}

func (p *Piece) Index() int         { return p.index }
func (p *Piece) Length() int64      { return p.length }
func (p *Piece) State() State       { return State(p.state.Load()) }
func (p *Piece) Complete() bool     { return p.State() == Verified }
func (p *Piece) Allocated() int64   { return p.alloc.Load() }
func (p *Piece) WriteCount() uint64 { return p.writes.Load() }
func (p *Piece) Accessed() int64    { return p.accessed.Load() }

func (p *Piece) chunkSize() int64 { return int64(p.owner.pool.ChunkSize()) }

// WriteAt stores engine data. Only fully covered units become readable by consumers.
func (p *Piece) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(b)) > p.length {
		return 0, ErrOutOfRange
	}
	if len(b) == 0 {
		return 0, nil
	}
	cs := p.chunkSize()
	p.mu.Lock()
	if p.owner.Closed() {
		p.mu.Unlock()
		return 0, ErrClosed
	}
	first := p.chunks == nil
	if first {
		p.chunks = make([][]byte, (p.length+cs-1)/cs)
		p.units = newUnitSet(int((p.length + unitSize - 1) / unitSize))
	}
	var grown int64
	for done := 0; done < len(b); {
		pos := off + int64(done)
		ci := pos / cs
		if p.chunks[ci] == nil {
			p.chunks[ci] = p.owner.pool.Get()
			grown += cs
		}
		done += copy(p.chunks[ci][pos%cs:], b[done:])
	}
	end := off + int64(len(b))
	for ui := (off + unitSize - 1) / unitSize; ; ui++ {
		start := ui * unitSize
		if start >= end || min(start+unitSize, p.length) > end {
			break
		}
		p.units.set(int(ui))
	}
	switch {
	case p.units.full(): // also a verified piece: its data changed after the hash
		p.state.Store(int32(Written))
	default:
		p.state.Store(int32(Partial))
	}
	p.writes.Add(1)
	p.accessed.Store(nowUnix())
	if grown > 0 {
		p.alloc.Add(grown)
		p.owner.filled.Add(grown)
	}
	p.mu.Unlock()
	p.owner.touchData()
	p.notify()
	if first {
		p.owner.dataArrived()
	}
	return len(b), nil
}

// Read serves a consumer from off: verified data only, or written units contiguous from off.
// Nothing available yet is ErrNotReady.
func (p *Piece) Read(b []byte, off int64, verifiedOnly bool) (int, error) {
	return p.read(b, off, ErrNotReady, func() int64 {
		if p.State() == Verified {
			return p.length - off
		}
		if verifiedOnly {
			return 0
		}
		ui := int(off / unitSize)
		run := p.units.runFrom(ui)
		if run == 0 {
			return 0
		}
		return min(int64(ui+run)*unitSize, p.length) - off
	})
}

// ReadAt serves the engine (hashing, uploads): allocated chunks contiguous from off. Nothing
// available (empty, released, closed) is exactly io.EOF: the engine wrapper compares with ==.
func (p *Piece) ReadAt(b []byte, off int64) (int, error) {
	return p.read(b, off, io.EOF, func() int64 {
		cs := p.chunkSize()
		end := off
		for ci := off / cs; ci < int64(len(p.chunks)) && p.chunks[ci] != nil; ci++ {
			end = min((ci+1)*cs, p.length)
		}
		return end - off
	})
}

func (p *Piece) read(b []byte, off int64, none error, available func() int64) (int, error) {
	if off < 0 || off > p.length {
		return 0, ErrOutOfRange
	}
	if len(b) == 0 {
		return 0, nil
	}
	if off == p.length {
		return 0, io.EOF
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.chunks == nil || p.owner.Closed() {
		return 0, none
	}
	want := min(int64(len(b)), available())
	if want <= 0 {
		return 0, none
	}
	cs := p.chunkSize()
	for done := int64(0); done < want; {
		pos := off + done
		done += int64(copy(b[done:want], p.chunks[pos/cs][pos%cs:]))
	}
	p.accessed.Store(nowUnix())
	return int(want), nil
}

// MarkComplete records a passed hash; the hash covered every byte, so every unit is readable.
func (p *Piece) MarkComplete() error {
	p.mu.Lock()
	ok := p.chunks != nil && !p.owner.Closed()
	for _, c := range p.chunks {
		if c == nil {
			ok = false
			break
		}
	}
	if ok {
		p.units.fill()
		p.state.Store(int32(Verified))
	}
	p.mu.Unlock()
	if !ok {
		return ErrNotReady
	}
	p.owner.touchData()
	p.notify()
	return nil
}

// markNotComplete reports whether data was held, i.e. a real hash failure rather than an
// evicted piece read back by the engine.
func (p *Piece) markNotComplete() bool {
	p.mu.Lock()
	had := p.chunks != nil && !p.owner.Closed()
	if had {
		p.units.reset()
		p.state.Store(int32(Partial))
	} else {
		p.state.Store(int32(Empty))
	}
	p.mu.Unlock()
	p.notify()
	return had
}

// Release frees the piece's memory back to the pool.
func (p *Piece) Release() int64 {
	p.mu.Lock()
	freed := p.releaseLocked(true)
	p.mu.Unlock()
	p.notify()
	return freed
}

// ReleaseIfUnwritten frees the piece only if no write landed since the write count `since`.
func (p *Piece) ReleaseIfUnwritten(since uint64) (int64, bool) {
	p.mu.Lock()
	if p.writes.Load() != since {
		p.mu.Unlock()
		return 0, false
	}
	freed := p.releaseLocked(true)
	p.mu.Unlock()
	p.notify()
	return freed, true
}

func (p *Piece) releaseIfVerified(writes uint64) (int64, bool) {
	p.mu.Lock()
	if p.State() != Verified || p.writes.Load() != writes {
		p.mu.Unlock()
		return 0, false
	}
	freed := p.releaseLocked(true)
	p.mu.Unlock()
	p.notify()
	return freed, true
}

func (p *Piece) drop() {
	p.mu.Lock()
	p.releaseLocked(false)
	p.mu.Unlock()
	p.notify()
}

func (p *Piece) releaseLocked(recycle bool) int64 {
	var freed int64
	for _, c := range p.chunks {
		if c != nil {
			if recycle && !p.owner.Closed() {
				p.owner.pool.Put(c)
			}
			freed += int64(cap(c))
		}
	}
	p.chunks = nil
	p.units = unitSet{}
	p.state.Store(int32(Empty))
	if freed > 0 {
		p.alloc.Add(-freed)
		p.owner.filled.Add(-freed)
	}
	return freed
}

// Changed returns a channel closed at the next change. Take it before reading, so a change
// between the read and the wait is not missed.
func (p *Piece) Changed() <-chan struct{} {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	// Checked under sigMu: Close sets closed before drop's notify takes sigMu.
	if p.owner.Closed() {
		return closedCh
	}
	if p.sig == nil {
		p.sig = make(chan struct{})
	}
	return p.sig
}

func (p *Piece) notify() {
	p.sigMu.Lock()
	if p.sig != nil {
		close(p.sig)
		p.sig = nil
	}
	p.sigMu.Unlock()
}
