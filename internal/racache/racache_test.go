package racache

import (
	"sync/atomic"
	"testing"

	"tiramisu/internal/config"
)

// newTest is a cache on cfg (nil: no configuration yet) with readers (nil: none open).
func newTest(cfg *config.Config, readers func(string) []Reader) *Cache {
	if readers == nil {
		readers = func(string) []Reader { return nil }
	}
	return New(Deps{
		Config:     func() *config.Config { return cfg },
		Logf:       func(string, ...any) {},
		Go:         func(f func()) { go f() },
		Responsive: func() bool { return false },
		Readers:    readers,
	})
}

func TestReadAheadCacheHoldsWalksChunksWithoutTouching(t *testing.T) {
	c := newTest(nil, nil)
	p := "/x.mkv"
	c.pieceLens.Store(p, int64(100))
	sh := c.getShard(p)
	sh.mu.Lock()
	for i := int64(0); i < 3; i++ {
		sh.buffers[c.chunkKey(p, i*100)] = &Buffer{start: i * 100, end: i*100 + 99}
	}
	sh.mu.Unlock()
	if !c.Holds(p, 50, 300) {
		t.Fatal("three contiguous chunks should hold [50, 300)")
	}
	if c.Holds(p, 50, 301) {
		t.Fatal("byte 300 is not cached")
	}
	for _, b := range sh.buffers {
		if b.lastAccess != 0 {
			t.Fatal("Holds must not touch lastAccess")
		}
	}
}

// A pump at its lead limit plus a second reader on the same file must not evict the chunks the
// primary player reads next: the analysis's own chunks behind it go first.
func TestReadAheadCacheEvictionKeepsPlayerNextChunk(t *testing.T) {
	const mb = int64(1 << 20)
	p := "/film.mkv"
	player := 189*mb + mb/4 // inside chunk 189
	var analysis int64
	c := newTest(&config.Config{ReadAheadBudget: 16 * mb, ReadAheadBase: mb}, func(path string) []Reader {
		if path != p {
			return nil
		}
		return []Reader{{Off: player, Primary: true}, {Off: atomic.LoadInt64(&analysis)}}
	})
	c.pieceLens.Store(p, mb)
	buf := make([]byte, mb)
	lead := 13 * mb // the pump's lead limit for this budget (pumpLeadLimit in internal/app)
	for off := 189 * mb; off <= player+lead; off += mb {
		c.Put(p, off, off+mb-1, buf)
	}
	for off := int64(0); off <= 9*mb; off += mb {
		atomic.StoreInt64(&analysis, off)
		c.Put(p, off, off+mb-1, buf)
	}
	for _, off := range []int64{player, 190 * mb} {
		if !c.Exists(p, off) {
			t.Fatalf("chunk at %dMB the player reads next was evicted", off/mb)
		}
	}
	if !c.Exists(p, 9*mb) {
		t.Fatal("the analysis's current chunk was evicted")
	}
	if c.Exists(p, 0) {
		t.Fatal("a chunk behind every reader survived while the player's were at risk")
	}
	if used := atomic.LoadInt64(&c.used); used > 16*mb {
		t.Fatalf("over budget: %dMB", used/mb)
	}
}
