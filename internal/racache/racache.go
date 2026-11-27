// Package racache is the read-ahead chunk cache between the torrent engine and FUSE reads.
package racache

import (
	"tiramisu/internal/config"

	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

// Deps is what the cache reads from the rest of the process; every field is required.
type Deps struct {
	Config     func() *config.Config // nil before the configuration loads
	Logf       func(format string, args ...any)
	Go         func(func())               // starts a background task (panic-safe in production)
	Responsive func() bool                // the engine serves unverified data
	Readers    func(path string) []Reader // open readers of a path, for eviction
}

// Buffer is one cached chunk of a file.
type Buffer struct {
	start, end     int64
	data           []byte
	lastAccess     int64
	sessionID      int64
	responsiveOnly bool // true if written in non-verified mode (responsive shield active)
}

// Cache is a 32-shard LRU cache with session-aware eviction.
type Cache struct {
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // recycled 16MB chunks

	muContext        sync.Mutex
	activePath       string
	currentSessionID int64
	isEvicting       int32 // atomic flag prevents concurrent global evictions

	pieceLens sync.Map // path → int64 adaptive chunk size (aligned to piece boundary)

	deps Deps
}

// Reader is one open handle's position as eviction sees it.
type Reader struct {
	Off     int64
	Primary bool
	Tail    bool // reading the SSD tail: needs nothing from the cache
}

// ForgetPieceLen drops the adaptive chunk size recorded for path.
func (c *Cache) ForgetPieceLen(path string) { c.pieceLens.Delete(path) }

// SetPieceLen stores the aligned chunk size for path derived from piece length.
// Called at pump start once pieceLen is known; both pump and raCache share this value.
func (c *Cache) SetPieceLen(path string, pl int64) {
	base := int64(c.deps.Config().ReadAheadBase)
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	if pl <= 0 {
		c.pieceLens.Delete(path)
		return
	}
	if n := base / pl; n > 0 {
		// pl fits within base: use the largest multiple of pl that's <= base, so chunk
		// boundaries always land on piece boundaries.
		c.pieceLens.Store(path, n*pl)
		return
	}
	// pl > base: chunk boundaries coincide with piece boundaries only at multiples of
	// lcm(base, pl) - with power-of-two pl and base (mandatory in BEP52/v2, merely
	// conventional in BEP3/v1, and the default ReadAheadBase) that's simply every
	// (pl/base)-th boundary. ~25% of the library uses non-power-of-two piece lengths
	// (see recycle() below), for which alignment is rarer still. Either way alignment
	// here is best-effort - reads are byte-range based regardless. Stored explicitly
	// (instead of falling through to ChunkSize's own base fallback) so pieceLens always
	// has an entry once pieceLen is known, and re-reads pick up base config changes at
	// pump start.
	c.pieceLens.Store(path, base)
}

// ChunkSize returns the adaptive chunk size for path (falls back to ReadAheadBase).
func (c *Cache) ChunkSize(path string) int64 {
	if v, ok := c.pieceLens.Load(path); ok {
		return v.(int64)
	}
	base := int64(c.deps.Config().ReadAheadBase)
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	return base
}

type raShard struct {
	mu      sync.RWMutex
	buffers map[string]*Buffer
	order   []string
	total   int64
}

func New(deps Deps) *Cache {
	if deps.Config == nil || deps.Logf == nil || deps.Go == nil || deps.Responsive == nil || deps.Readers == nil {
		panic("racache: every Deps field is required")
	}
	// Static 32-buffer default: New runs at package init (app's var raCache),
	// BEFORE config is loaded - Config() returns nil here, so nothing config-dependent may be
	// touched. The pool is re-anchored to the budget once config exists via
	// AnchorPoolToConfig() (called from main after loading).
	c := &Cache{
		deps:      deps,
		shardMask: 31,
		pool:      make(chan []byte, 32), // Cap at 32 chunks (512MB max pool)
	}
	for i := range c.shards {
		c.shards[i] = &raShard{
			buffers: make(map[string]*Buffer),
		}
	}
	return c
}

// AnchorPoolToConfig re-sizes the recycled-buffer pool to the configured budget. Idle buffers
// are only ever needed to absorb eviction/overwrite storms, whose size scales with how much
// live content the budget allows - previously a fixed 32 (512MB max pool) regardless of
// budget: wasteful at 128MB (8 live chunks, 512MB parked), and at 512MB (32 live chunks) the
// 32-cap was 1:1 with live content, leaving no margin for the 32-shard concurrent-pop burst
// (every Put pops one buffer before its eviction recycle lands - with all shards popping
// simultaneously the pool must hold live-count buffers or allocations churn).
// clamp(budget/base*2, 16, 64): 2x live content so the margin holds across the whole range
// (256MB->32, 512MB->64), floor 16 to avoid allocation churn on tiny budgets, ceiling 64 to
// bound parking. The ceiling is nearly free: the pool only ever retains buffers that were
// already allocated, it never pre-allocates.
func (c *Cache) AnchorPoolToConfig() {
	base := c.deps.Config().ReadAheadBase
	if base <= 0 {
		base = 16 * 1024 * 1024
	}
	budget := c.deps.Config().ReadAheadBudget
	if budget <= 0 {
		budget = 256 * 1024 * 1024
	}
	poolCap := int(budget / base * 2)
	if poolCap < 16 {
		poolCap = 16
	}
	if poolCap > 64 {
		poolCap = 64
	}
	if poolCap == cap(c.pool) {
		return
	}
	c.deps.Logf("[RaCache] Pool cap anchored to budget: %d buffers (%dMB max, budget %dMB, base %dMB)",
		poolCap, poolCap*int(base)/1024/1024, budget/1024/1024, base/1024/1024)
	c.pool = make(chan []byte, poolCap)
}

func (c *Cache) getShard(path string) *raShard {
	return c.shards[xxhash.Sum64String(path)&c.shardMask]
}

// allocChunk allocates a chunk buffer with capacity rounded up to base so recycle() can pool
// it regardless of the file's adaptive chunk size. Sizes above base (shouldn't happen for
// pump chunks) fall back to an exact, unpoolable allocation.
func allocChunk(size, base int64) []byte {
	if size < base {
		return make([]byte, size, base)
	}
	return make([]byte, size)
}

func (c *Cache) recycle(b []byte) {
	chunkSize := int(16 * 1024 * 1024)
	if c.deps.Config().ReadAheadBase > 0 {
		chunkSize = int(c.deps.Config().ReadAheadBase)
	}
	// Match on cap, not len: adaptive chunks (see SetPieceLen) have len < base for torrents
	// whose piece length doesn't divide base evenly (~25% of the library uses non-power-of-two
	// piece lengths), but Put always allocates them with cap == base so they stay poolable.
	if cap(b) == chunkSize {
		select {
		case c.pool <- b[:chunkSize]:
		default:
			// Pool full, let GC handle it
		}
	}
}

// MaxCachedOffset returns the highest cached byte end for a given path.
func (c *Cache) MaxCachedOffset(p string) int64 {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := p + ":"
	var maxEnd int64
	for key, b := range s.buffers {
		if strings.HasPrefix(key, prefix) && b.end > maxEnd {
			maxEnd = b.end
		}
	}
	return maxEnd

}

// chunkKey returns a compound key using the per-path adaptive chunk size.
func (c *Cache) chunkKey(path string, offset int64) string {
	return fmt.Sprintf("%s:%d", path, offset/c.ChunkSize(path))
}

// SwitchContext increments SessionID on path change to invalidate stale data.
func (c *Cache) SwitchContext(newPath string) {
	c.muContext.Lock()
	pathChanged := newPath != c.activePath
	if pathChanged {
		c.activePath = newPath
		// Increment session ID: All old buffers (with old ID) become "stale" instantly
		c.currentSessionID++
	}
	activePath := c.activePath
	activeSessionID := c.currentSessionID
	c.muContext.Unlock()

	if pathChanged {
		c.deps.Go(func() {
			c.triggerGlobalEviction(activePath, activeSessionID)
		})
	}
}

// Holds reports whether [off, end) is cached, walking any number of chunks. Read-only: unlike
// Covered it leaves lastAccess alone, so the dashboard can ask without affecting eviction.
func (c *Cache) Holds(p string, off, end int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for pos := off; pos < end; {
		b, ok := s.buffers[c.chunkKey(p, pos)]
		if !ok || pos < b.start || pos > b.end {
			return false
		}
		pos = b.end + 1
	}
	return true
}

// Covered reports whether the cache fully covers [off, end] without copying any bytes —
// use this for presence/coverage checks on the hot path; call Get separately only once the
// actual data is needed, to avoid an up-to-16MB defensive copy on every check.
func (c *Cache) Covered(p string, off, end int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		if end <= b.end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			return true
		}
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			return true
		}
	}
	return false
}

func (c *Cache) Get(p string, off, end int64) []byte {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Defensive copy: pool evicts buffers immediately; sub-slice reference causes use-after-free.
			src := b.data[off-b.start : end-b.start+1]
			out := make([]byte, len(src))
			copy(out, src)
			return out
		}
		// Cross-boundary read: stitch two adjacent chunks to avoid FetchBlock on chunk boundary straddles.
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			out := make([]byte, end-off+1)
			n1 := copy(out, b.data[off-b.start:])
			copy(out[n1:], b2.data[:end-b2.start+1])
			return out
		}
	}
	return nil
}

// Exists checks if a chunk is present in cache without allocating.
func (c *Cache) Exists(p string, off int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	_, found := s.buffers[key]
	return found
}

// CopyTo copies data directly into dest, avoiding an intermediate allocation in the FUSE Read hot path.
func (c *Cache) CopyTo(p string, off, end int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Fast path: entirely within one chunk.
			src := b.data[off-b.start : end-b.start+1]
			return copy(dest, src)
		}
		// Cross-boundary read: same logic as Get().
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			n1 := copy(dest, b.data[off-b.start:])
			n2 := copy(dest[n1:], b2.data[:end-b2.start+1])
			return n1 + n2
		}
	}
	return 0
}

func (c *Cache) Put(p string, start, end int64, d []byte) {
	c.muContext.Lock()
	sessID := c.currentSessionID
	// Active path uses current session ID; stale pumps use sessID=0 so eviction can identify them.
	if p != c.activePath {
		sessID = 0
	}
	c.muContext.Unlock()

	shard := c.getShard(p)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	key := c.chunkKey(p, start)

	dataSize := int64(len(d))

	// Pooled buffers all have cap == ReadAheadBase; adaptive chunk sizes are always <= base
	// (guaranteed by SetPieceLen), so a pooled buffer resliced to dataSize serves any chunk.
	// Fresh allocations get cap rounded up to base too, so recycle() can pool them later —
	// previously a size mismatch both discarded the pooled buffer and allocated an
	// unpoolable one, permanently draining the pool on non-power-of-two-piece torrents.
	poolCap := int64(c.deps.Config().ReadAheadBase)
	if poolCap == 0 {
		poolCap = 16 * 1024 * 1024
	}
	var dataCopy []byte
	select {
	case buf := <-c.pool:
		if int64(cap(buf)) >= dataSize {
			dataCopy = buf[:dataSize]
		} else {
			// Buffer predates a smaller base config; too small now, let GC take it.
			dataCopy = allocChunk(dataSize, poolCap)
		}
	default:
		dataCopy = allocChunk(dataSize, poolCap)
	}
	copy(dataCopy, d)

	globalLimit := c.deps.Config().ReadAheadBudget
	if globalLimit <= 0 {
		globalLimit = 256 * 1024 * 1024 // Fail-safe default
	}

	// 1. Account for overwrite
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		c.recycle(old.data)
		// Promote to MRU on overwrite to prevent premature eviction of recently refreshed chunks.
		for i, k := range shard.order {
			if k == key {
				shard.order = append(shard.order[:i], shard.order[i+1:]...)
				break
			}
		}
	}
	shard.order = append(shard.order, key)

	// 2. Add new data
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	responsiveOnly := c.deps.Responsive()
	shard.buffers[key] = &Buffer{start, end, dataCopy, time.Now().UnixNano(), sessID, responsiveOnly}

	// Evict from this shard while over budget, least needed first; never the chunk just added.
	exhausted := false
	if newUsed > globalLimit {
		readers := make(map[string]raPathReaders)
		for newUsed > globalLimit {
			i := c.pickVictim(shard, key, globalLimit, readers)
			if i < 0 {
				exhausted = true
				break
			}
			v := shard.order[i]
			shard.order = append(shard.order[:i], shard.order[i+1:]...)
			old := shard.buffers[v]
			evictedSize := int64(len(old.data))
			shard.total -= evictedSize
			delete(shard.buffers, v)
			newUsed = atomic.AddInt64(&c.used, -evictedSize)
			c.recycle(old.data)
		}
	}

	// Local shard exhausted: trigger global eviction to free stale data from other shards.
	if newUsed > globalLimit && exhausted {
		c.muContext.Lock()
		ap := c.activePath
		sid := c.currentSessionID
		c.muContext.Unlock()
		c.deps.Go(func() {
			c.triggerGlobalEviction(ap, sid)
		})
	}

	if newUsed > globalLimit && len(shard.order) == 0 {
		// If we reach here, it means even after global eviction we are over budget.
		// However, to allow the new stream to start, we must NOT drop the only chunk it has.
		// So we do nothing and allow a tiny over-budget for the first few chunks.
		c.deps.Logf("[RaCache] Budget full (%d MB), allowing active chunk to persist for handoff: %s", newUsed/(1024*1024), filepath.Base(p))
	}

	// Periodic compaction of order slice
	if len(shard.order) > 100 && len(shard.order) > len(shard.buffers)*2 {
		newOrder := make([]string, 0, len(shard.buffers))
		for _, v := range shard.order {
			if _, exists := shard.buffers[v]; exists {
				newOrder = append(newOrder, v)
			}
		}
		shard.order = newOrder
	}
}

// raPathReaders is one path's open readers, resolved once per eviction pass.
type raPathReaders struct {
	open          bool  // any handle open on the path
	anchor        int64 // primary player position as the pump follows it
	anchorUnknown bool  // the only primaries read the SSD tail
	secondaries   []int64
}

func (c *Cache) pathReaders(path string) raPathReaders {
	src := c.deps.Readers
	var pr raPathReaders
	primary, tailPrimary := false, false
	for _, r := range src(path) {
		pr.open = true
		off := max(r.Off, 0)
		switch {
		case r.Tail:
			if r.Primary {
				tailPrimary = true
			}
		case r.Primary:
			primary = true
			pr.anchor = max(pr.anchor, off)
		default:
			pr.secondaries = append(pr.secondaries, off)
		}
	}
	// Mirrors the pump: no streaming primary means it fills from 0, a tail-only one hides its position.
	pr.anchorUnknown = !primary && tailPrimary
	return pr
}

// Eviction tiers, lowest evicted first.
const (
	raTierUnneeded      = iota // no open handle, behind every reader, or beyond what any reader needs
	raTierPrimaryAhead         // pump lead ahead of the primary, farthest first
	raTierSecondaryNext        // the chunk a secondary is reading and the next one
	raTierPrimaryNext          // the chunk the primary is reading and the next one
)

// pickVictim returns the index in s.order of the chunk to evict, or -1 if only keep is left.
func (c *Cache) pickVictim(s *raShard, keep string, budget int64, cache map[string]raPathReaders) int {
	best, bestTier, bestStart := -1, 0, int64(0)
	for i := 0; i < len(s.order); i++ {
		k := s.order[i]
		b, ok := s.buffers[k]
		if !ok {
			s.order = append(s.order[:i], s.order[i+1:]...)
			i--
			continue
		}
		if k == keep {
			continue
		}
		path := k[:strings.LastIndexByte(k, ':')]
		pr, ok := cache[path]
		if !ok {
			pr = c.pathReaders(path)
			cache[path] = pr
		}
		tier := raChunkTier(pr, b.start, b.end, c.ChunkSize(path), budget)
		if tier == raTierUnneeded {
			return i // oldest unneeded chunk
		}
		if best < 0 || tier < bestTier || (tier == bestTier && tier == raTierPrimaryAhead && b.start > bestStart) {
			best, bestTier, bestStart = i, tier, b.start
		}
	}
	return best
}

func raChunkTier(pr raPathReaders, start, end, chunk, budget int64) int {
	if !pr.open {
		return raTierUnneeded
	}
	if pr.anchorUnknown {
		return raTierPrimaryAhead
	}
	tier := raTierUnneeded
	if end >= pr.anchor && start < pr.anchor+2*chunk {
		return raTierPrimaryNext
	}
	if end >= pr.anchor && start <= pr.anchor+budget {
		tier = raTierPrimaryAhead
	}
	for _, off := range pr.secondaries {
		if end >= off && start < off+2*chunk {
			return raTierSecondaryNext
		}
	}
	return tier
}

// Stats returns read-ahead cache statistics for metrics endpoint
func (c *Cache) Stats() (totalBytes int64, activeBytes int64, entries int) {
	totalBytes = atomic.LoadInt64(&c.used)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	for _, shard := range c.shards {
		shard.mu.RLock()
		entries += len(shard.buffers)
		for _, buf := range shard.buffers {
			if now-atomic.LoadInt64(&buf.lastAccess) < staleThreshold {
				activeBytes += int64(len(buf.data))
			}
		}
		shard.mu.RUnlock()
	}

	return totalBytes, activeBytes, entries
}

// triggerGlobalEviction removes stale session data and old chunks from all shards.
func (c *Cache) triggerGlobalEviction(activePath string, activeSessionID int64) {
	// Single-flight: skip if already evicting.
	if !atomic.CompareAndSwapInt32(&c.isEvicting, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.isEvicting, 0)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	evictShard := func(s *raShard) {
		var newOrder []string
		for _, key := range s.order {
			keep := true
			if buf, ok := s.buffers[key]; ok {
				// 1. Session ID Check (Fastest)
				if buf.sessionID != activeSessionID && !strings.HasPrefix(key, activePath+":") {
					keep = false
				} else {
					threshold := staleThreshold
					if strings.HasPrefix(key, activePath+":") {
						threshold = (60 * time.Second).Nanoseconds()
					}

					lastAcc := atomic.LoadInt64(&buf.lastAccess)
					if now-lastAcc > threshold {
						keep = false
					}
				}

				if !keep {
					size := int64(len(buf.data))
					s.total -= size
					delete(s.buffers, key)
					atomic.AddInt64(&c.used, -size)
					c.recycle(buf.data)
				}
			}

			if keep {
				newOrder = append(newOrder, key)
			}
		}
		s.order = newOrder
	}

	skipped := 0
	for _, s := range c.shards {
		if !s.mu.TryLock() {
			skipped++
			continue
		}
		evictShard(s)
		s.mu.Unlock()
	}

	// If all shards were busy, force blocking eviction on first shard to prevent budget overflow.
	if skipped == len(c.shards) {
		s := c.shards[0]
		s.mu.Lock()
		evictShard(s)
		s.mu.Unlock()
	}
}

// Drop forgets every chunk of path overlapping [off, end): bytes of a piece that failed its hash.
func (c *Cache) Drop(p string, off, end int64) {
	s := c.getShard(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := p + ":"
	for key, b := range s.buffers {
		if !strings.HasPrefix(key, prefix) || b.end < off || b.start >= end {
			continue
		}
		delete(s.buffers, key)
		s.total -= int64(len(b.data))
		atomic.AddInt64(&c.used, -int64(len(b.data)))
		c.recycle(b.data)
		if i := slices.Index(s.order, key); i >= 0 {
			s.order = slices.Delete(s.order, i, i+1)
		}
	}
}

// CopyAny copies into dest the bytes at off from whichever cached chunk of path holds off,
// whatever offset that chunk was stored from (CopyTo only looks under off's own chunk key); it
// returns how many contiguous bytes it copied from that one chunk.
func (c *Cache) CopyAny(p string, off int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.buffers[c.chunkKey(p, off)]; ok && off >= b.start && off <= b.end {
		return copy(dest, b.data[off-b.start:])
	}
	prefix := p + ":"
	for key, b := range s.buffers {
		if off >= b.start && off <= b.end && strings.HasPrefix(key, prefix) {
			return copy(dest, b.data[off-b.start:])
		}
	}
	return 0
}
