package torrstor

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// V96: Global buffer pool to reuse memory pieces and reduce GC pressure
var memBufferPool = sync.Pool{
	New: func() interface{} {
		return nil // Return nil so we can handle specific piece length allocations
	},
}

// memBufferPut is the only way a buffer re-enters the shared pool; a var so tests can observe
// what gets recycled.
var memBufferPut = func(b []byte) { memBufferPool.Put(b) }

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

		// V96: Try to get a buffer from the pool
		if pooledBuf := memBufferPool.Get(); pooledBuf != nil {
			buf := pooledBuf.([]byte)
			// Check if the pooled buffer has the correct capacity
			if int64(len(buf)) == p.piece.cache.pieceLength {
				p.buffer = buf
			}
		}

		// If pool was empty or size mismatch, allocate once
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
		// V96: Put the buffer back into the pool for future reuse
		memBufferPut(p.buffer)
		p.buffer = nil
	}
	atomic.StoreInt64(&p.piece.Size, 0)
	p.piece.Complete.Store(false)
}

// drop frees the buffer without recycling it. The pool is global and holds no notion of piece
// length: a Get that draws the wrong size discards the buffer and allocates anyway, so handing
// it a closing torrent's buffers makes every allocation of the torrent still playing miss. Only
// same-cache eviction, where the length always matches, may recycle.
func (p *MemPiece) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffer = nil
	atomic.StoreInt64(&p.piece.Size, 0)
	p.piece.Complete.Store(false)
}
