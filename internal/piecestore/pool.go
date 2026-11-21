package piecestore

import (
	"sync"
	"sync/atomic"
)

// DefaultChunkSize is the allocation unit of piece memory.
const DefaultChunkSize = 256 << 10

// ChunkPool hands out fixed-size buffers and keeps some for reuse. A plain list, not a
// sync.Pool: a GC every few seconds would empty it. Buffers are not zeroed.
type ChunkPool struct {
	size    int
	maxFree int

	mu   sync.Mutex
	free [][]byte

	allocs atomic.Int64
	reuses atomic.Int64
}

// NewChunkPool keeps at most maxFreeBytes of idle chunks (never fewer than 2).
func NewChunkPool(chunkSize int, maxFreeBytes int64) *ChunkPool {
	if chunkSize <= 0 || chunkSize%unitSize != 0 {
		panic("piecestore: chunk size must be a positive multiple of 1KiB")
	}
	n := int(maxFreeBytes / int64(chunkSize))
	if n < 2 {
		n = 2
	}
	return &ChunkPool{size: chunkSize, maxFree: n}
}

func (p *ChunkPool) ChunkSize() int { return p.size }

func (p *ChunkPool) Get() []byte {
	p.mu.Lock()
	if k := len(p.free); k > 0 {
		b := p.free[k-1]
		p.free[k-1] = nil
		p.free = p.free[:k-1]
		p.mu.Unlock()
		p.reuses.Add(1)
		return b
	}
	p.mu.Unlock()
	p.allocs.Add(1)
	return make([]byte, p.size)
}

func (p *ChunkPool) Put(b []byte) {
	if cap(b) != p.size {
		return
	}
	p.mu.Lock()
	if len(p.free) < p.maxFree {
		p.free = append(p.free, b[:p.size])
	}
	p.mu.Unlock()
}

// Stats returns allocations, reuses and idle chunks.
func (p *ChunkPool) Stats() (allocs, reuses int64, free int) {
	p.mu.Lock()
	free = len(p.free)
	p.mu.Unlock()
	return p.allocs.Load(), p.reuses.Load(), free
}
