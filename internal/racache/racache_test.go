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

func TestDropForgetsOverlappingChunksOfPathOnly(t *testing.T) {
	c := newTest(&config.Config{ReadAheadBudget: 1 << 30}, nil)
	p, other := "/a.mkv", "/b.mkv"
	c.pieceLens.Store(p, int64(100))
	c.pieceLens.Store(other, int64(100))
	for i := int64(0); i < 4; i++ {
		c.Put(p, i*100, i*100+99, make([]byte, 100))
	}
	c.Put(other, 100, 199, make([]byte, 100))
	c.Drop(p, 150, 250) // chunks 1 and 2
	for i, want := range []bool{true, false, false, true} {
		if got := c.Covered(p, int64(i)*100, int64(i)*100+99); got != want {
			t.Errorf("chunk %d cached %v, want %v", i, got, want)
		}
	}
	if !c.Covered(other, 100, 199) {
		t.Error("another path lost its chunk")
	}
	if _, _, n := c.Stats(); n != 3 {
		t.Errorf("%d entries, want 3", n)
	}
}

// A chunk stored from an offset in an earlier key still serves the bytes it holds.
func TestCopyAnyFindsChunkStoredFromEarlierKey(t *testing.T) {
	c := newTest(&config.Config{ReadAheadBudget: 1 << 30}, nil)
	p := "/a.mkv"
	c.pieceLens.Store(p, int64(100))
	data := make([]byte, 250)
	for i := range data {
		data[i] = byte(i)
	}
	c.Put(p, 50, 299, data) // keyed under chunk 0, holds 50..299
	dest := make([]byte, 10)
	if n := c.CopyTo(p, 220, 229, dest); n != 0 {
		t.Fatalf("CopyTo found it after all (%d): the test no longer shows the gap", n)
	}
	if n := c.CopyAny(p, 220, dest); n != 10 || dest[0] != 170 {
		t.Fatalf("CopyAny: n=%d first=%d", n, dest[0])
	}
}
