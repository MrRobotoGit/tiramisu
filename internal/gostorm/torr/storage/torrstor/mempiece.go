package torrstor

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type MemPiece struct {
	piece *Piece

	buffer []byte
	mu     sync.RWMutex
}

func NewMemPiece(p *Piece) *MemPiece {
	return &MemPiece{piece: p}
}

func (p *MemPiece) WriteAt(b []byte, off int64) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.buffer == nil {
		// V227: Non-blocking rate-limited cleanup trigger
		select {
		case p.piece.cache.cleanTrigger <- struct{}{}:
		default:
		}

		p.buffer = p.piece.cache.getBuffer()
		if p.buffer == nil {
			p.buffer = make([]byte, p.piece.cache.pieceLength, p.piece.cache.pieceLength)
		}
	}
	n = copy(p.buffer[off:], b[:])
	// Size is read by cleanPieces/GetState under the cache lock, written here under the piece
	// lock: two different mutexes, so the accesses have to be atomic.
	if sz := atomic.AddInt64(&p.piece.Size, int64(n)); sz > p.piece.cache.pieceLength {
		atomic.StoreInt64(&p.piece.Size, p.piece.cache.pieceLength)
	}
	atomic.StoreInt64(&p.piece.Accessed, time.Now().Unix())
	return
}

func (p *MemPiece) ReadAt(b []byte, off int64) (n int, err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	size := len(b)
	if size+int(off) > len(p.buffer) {
		size = len(p.buffer) - int(off)
		if size < 0 {
			size = 0
		}
	}
	if len(p.buffer) < int(off) || len(p.buffer) < int(off)+size {
		return 0, io.EOF
	}
	n = copy(b, p.buffer[int(off) : int(off)+size][:])
	atomic.StoreInt64(&p.piece.Accessed, time.Now().Unix())
	if int64(len(b))+off >= atomic.LoadInt64(&p.piece.Size) {
		// V227: Non-blocking rate-limited cleanup trigger
		select {
		case p.piece.cache.cleanTrigger <- struct{}{}:
		default:
		}
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (p *MemPiece) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buffer != nil {
		p.piece.cache.putBuffer(p.buffer)
		p.buffer = nil
	}
	atomic.StoreInt64(&p.piece.Size, 0)
	p.piece.Complete.Store(false)
}

// drop frees the buffer without recycling it, for a cache that is closing: its freelist is
// discarded in the same breath, so keeping the buffer would only delay the collector.
func (p *MemPiece) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffer = nil
	atomic.StoreInt64(&p.piece.Size, 0)
	p.piece.Complete.Store(false)
}
