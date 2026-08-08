package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"tiramisu/internal/warmup"
)

// ttffSource identifies which Read() path served the data. Used as histogram key.
type ttffSource uint8

const (
	srcWarmupHead ttffSource = iota
	srcWarmupTail
	srcRACacheHit
	srcFetchBlock
)

func (s ttffSource) String() string {
	switch s {
	case srcWarmupHead:
		return "warmup_head"
	case srcWarmupTail:
		return "warmup_tail"
	case srcRACacheHit:
		return "racache_hit"
	case srcFetchBlock:
		return "fetch_block"
	}
	return "unknown"
}

// Fixed logarithmic bucket upper bounds, milliseconds. Bucket i covers
// (buckets[i-1], buckets[i]]; the final implicit bucket is [10000,∞).
var ttffBucketsMS = [...]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 3000, 10000}

// TTFFHistogram is a fixed-bucket latency histogram with zero allocations.
// Percentiles are computed on demand by a cumulative scan (O(len(buckets))).
type TTFFHistogram struct {
	buckets [len(ttffBucketsMS) + 1]atomic.Int64
	total   atomic.Int64
	sumMS   atomic.Int64
	maxMS   atomic.Int64
}

func (h *TTFFHistogram) Add(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	idx := 0
	for idx < len(ttffBucketsMS) && ms > ttffBucketsMS[idx] {
		idx++
	}
	h.buckets[idx].Add(1)
	h.total.Add(1)
	h.sumMS.Add(ms)
	for {
		cur := h.maxMS.Load()
		if ms <= cur || h.maxMS.CompareAndSwap(cur, ms) {
			break
		}
	}
}

func (h *TTFFHistogram) Count() int64 { return h.total.Load() }

func (h *TTFFHistogram) Avg() float64 {
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	return float64(h.sumMS.Load()) / float64(total)
}

func (h *TTFFHistogram) Max() float64 { return float64(h.maxMS.Load()) }

// Pct returns the bucket upper bound (ms) at which the cumulative count
// reaches p*100% of samples. p must be in [0,1]; the result is an
// approximate snapshot under concurrent Add. The final bucket reports
// Max() as representative.
func (h *TTFFHistogram) Pct(p float64) float64 {
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	need := int64(p * float64(total))
	var cum int64
	for i := 0; i < len(h.buckets); i++ {
		cum += h.buckets[i].Load()
		if cum >= need {
			if i < len(ttffBucketsMS) {
				return float64(ttffBucketsMS[i])
			}
			return float64(h.maxMS.Load())
		}
	}
	return 0
}

const (
	ttffIdleClose     = 60 * time.Second
	ttffMaxLife       = 30 * time.Minute
	ttffProbeMinRead  = int64(1 << 20)  // 1MB: Plex probes never read this much
	ttffTailMinRead   = int64(64 << 10) // 64KB: scanners read at most one Cues probe chunk
	ttffDeepReadOff   = int64(8 << 20)  // 8MB: header/seek probes go deeper
	ttffStallDuration = time.Second
)

// TTFFSession tracks one playback open of a virtual MKV (per-path, shared by
// primary/secondary handles like playbackRegistry).
type TTFFSession struct {
	path            string
	size            int64
	hash            string
	warmupHeadReady atomic.Bool
	warmupTailReady atomic.Bool

	openedAt     atomic.Int64 // unixNano
	firstDataAt  atomic.Int64 // 0 = unset (CAS-once)
	reached8MBAt atomic.Int64 // 0 = unset (CAS-once)
	lastReadAt   atomic.Int64
	bytesRead    atomic.Int64
	lastOff      atomic.Int64 // start offset of previous read; -1 = none
	seekCount    atomic.Int64
	stallCount   atomic.Int64
	maxStallMS   atomic.Int64
	tailOrDeep   atomic.Bool // read at off>=8MB or inside last 16MB (tail region)

	closeOnce sync.Once
	closed    atomic.Bool
}

// recordRead updates session state for one served FUSE read. lastReadAt is
// refreshed on every served read.
func (s *TTFFSession) recordRead(d time.Duration, n int, off int64) {
	if s.closed.Load() {
		return
	}
	nowN := time.Now().UnixNano()
	s.lastReadAt.Store(nowN)
	if n > 0 {
		s.firstDataAt.CompareAndSwap(0, nowN)
		s.bytesRead.Add(int64(n))
		if off >= ttffDeepReadOff || (s.size > 0 && off >= s.size-warmup.TailWarmupSize) {
			s.tailOrDeep.Store(true)
			s.reached8MBAt.CompareAndSwap(0, nowN)
		}
		if prev := s.lastOff.Load(); prev > 0 {
			if b := gc(); b != nil && shouldInterruptForSeek(prev, off, b.ReadAheadBudget) {
				s.seekCount.Add(1)
				ttffStats.seekLatency.Add(d)
			}
		} else if prev == 0 {
			// Previous data read was the header at offset 0 — a served read,
			// not an uninitialized offset (those are seeded -1 by ttffRegister,
			// and shouldInterruptForSeek ignores prevOff<=0). A jump beyond the
			// read-ahead budget from the header is a real seek.
			if b := gc(); b != nil && off > b.ReadAheadBudget {
				s.seekCount.Add(1)
				ttffStats.seekLatency.Add(d)
			}
		}
		s.lastOff.Store(off)
	}
	if d > ttffStallDuration {
		s.stallCount.Add(1)
		ms := d.Milliseconds()
		for {
			cur := s.maxStallMS.Load()
			if ms <= cur || s.maxStallMS.CompareAndSwap(cur, ms) {
				break
			}
		}
	}
}

// ttffIsReal decides at close time whether a session was a real playback
// (vs a Plex/Samba scan probe). Scanners read almost nothing — even a tail
// Cues probe stays within one chunk (see tailProbeZoneSize docs in main.go);
// any 1MB+ of data, or a tail/deep read of more than 64KB, is playback.
func ttffIsReal(tailOrDeep bool, bytesRead int64) bool {
	if bytesRead >= ttffProbeMinRead {
		return true
	}
	return tailOrDeep && bytesRead > ttffTailMinRead
}

// closeSession aggregates the session into the global histograms and logs one
// [TTFF] line. Idempotent via closeOnce. Removes itself from the registry.
func (s *TTFFSession) closeSession() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		sessions.CompareAndDelete(s.path, s)
		if !ttffIsReal(s.tailOrDeep.Load(), s.bytesRead.Load()) {
			ttffStats.sessionsFiltered.Add(1)
			return
		}
		ttffStats.sessionsCompleted.Add(1)
		opened := s.openedAt.Load()
		if fd := s.firstDataAt.Load(); fd > 0 {
			ttffStats.openToHeader.Add(time.Duration(fd - opened))
		}
		if r8 := s.reached8MBAt.Load(); r8 > 0 {
			ttffStats.openTo8MB.Add(time.Duration(r8 - opened))
		}
		if sc := s.stallCount.Load(); sc > 0 {
			ttffStats.stallCount.Add(sc)
		}
		if ms := s.maxStallMS.Load(); ms > 0 {
			for {
				cur := ttffStats.maxStallMS.Load()
				if ms <= cur || ttffStats.maxStallMS.CompareAndSwap(cur, ms) {
					break
				}
			}
		}
		logger.Printf("[TTFF] path=%s head=%d tail=%d header=%s to8mb=%s seek=%d stalls=%d bytes=%d",
			filepath.Base(s.path),
			boolToInt(s.warmupHeadReady.Load()), boolToInt(s.warmupTailReady.Load()),
			durOrDash(s.firstDataAt.Load(), opened), durOrDash(s.reached8MBAt.Load(), opened),
			s.seekCount.Load(), s.stallCount.Load(), s.bytesRead.Load())
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func durOrDash(t, opened int64) string {
	if t == 0 {
		return "-"
	}
	d := time.Duration(t - opened)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// --- Registry ---

var sessions sync.Map // path -> *TTFFSession

type ttffAgg struct {
	sessionsCompleted atomic.Int64
	sessionsFiltered  atomic.Int64
	openToHeader      TTFFHistogram
	openTo8MB         TTFFHistogram
	seekLatency       TTFFHistogram
	warmupHead        TTFFHistogram
	warmupTail        TTFFHistogram
	raCacheHit        TTFFHistogram
	fetchBlock        TTFFHistogram
	stallCount        atomic.Int64
	maxStallMS        atomic.Int64
}

var ttffStats ttffAgg

// ttffRegister opens (or reopens) the session for a path. Reopening refreshes
// lastReadAt and the warmup flags, but openedAt is refreshed only while the
// session has not served data yet (probe-then-player): refreshing it after the
// first read would produce a negative or distorted openToHeader (observed
// header=-75ms: a second Open mid-session refreshed openedAt past firstDataAt).
// Non-torrent files (hash=="") are skipped.
func ttffRegister(path string, size int64, hash string, headReady, tailReady bool) {
	if hash == "" {
		return
	}
	nowN := time.Now().UnixNano()
	ns := &TTFFSession{path: path, size: size, hash: hash}
	ns.warmupHeadReady.Store(headReady)
	ns.warmupTailReady.Store(tailReady)
	ns.openedAt.Store(nowN)
	ns.lastReadAt.Store(nowN)
	ns.lastOff.Store(-1)
	if actual, loaded := sessions.LoadOrStore(path, ns); loaded {
		s := actual.(*TTFFSession)
		if s.closed.Load() {
			sessions.Store(path, ns) // resurrect after close
			return
		}
		if s.firstDataAt.Load() == 0 {
			s.openedAt.Store(nowN)
			s.lastOff.Store(-1)
		}
		s.lastReadAt.Store(nowN)
		s.warmupHeadReady.Store(headReady)
		s.warmupTailReady.Store(tailReady)
	}
}

// ttffRead records one served read: per-source histogram always; session state
// when a session exists for the path.
func ttffRead(path string, src ttffSource, d time.Duration, n int, off int64) {
	switch src {
	case srcWarmupHead:
		ttffStats.warmupHead.Add(d)
	case srcWarmupTail:
		ttffStats.warmupTail.Add(d)
	case srcRACacheHit:
		ttffStats.raCacheHit.Add(d)
	case srcFetchBlock:
		ttffStats.fetchBlock.Add(d)
	}
	if val, ok := sessions.Load(path); ok {
		val.(*TTFFSession).recordRead(d, n, off)
	}
}

// ttffReleaseClose closes the session immediately on Release when nothing was
// ever read (scan probe). Guarded by the open-handle tracker: a probe handle
// releasing while a player has the same path open (even with 0 bytes read —
// cold-start Wake window) must not kill the player's session; those sessions
// are reaped by ttffCleanupLoop instead. Note: at this point the releasing
// handle is still counted by the tracker (Dec runs later in Release), so only
// slot-less probes close immediately — exactly the intended behavior.
func ttffReleaseClose(path string) {
	if globalOpenTracker.IsPathOpen(path) {
		return
	}
	if val, ok := sessions.Load(path); ok {
		if s := val.(*TTFFSession); s.bytesRead.Load() == 0 {
			s.closeSession()
		}
	}
}

// ttffCleanupLoop closes sessions idle for ttffIdleClose, or idle beyond 30s
// once older than ttffMaxLife (zombie handles with periodic probe reads);
// active playbacks are never closed by age.
func ttffCleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		nowN := time.Now().UnixNano()
		sessions.Range(func(key, val interface{}) bool {
			s := val.(*TTFFSession)
			idle := nowN - s.lastReadAt.Load()
			if idle > int64(ttffIdleClose) ||
				(idle > int64(30*time.Second) && nowN-s.openedAt.Load() > int64(ttffMaxLife)) {
				s.closeSession()
			}
			return true
		})
	}
}

// histJSON renders a histogram as JSON for /metrics/ttff.
func histJSON(h *TTFFHistogram) string {
	return fmt.Sprintf(`{"avg":%.0f,"p50":%.0f,"p95":%.0f,"p99":%.0f,"max":%.0f,"count":%d}`,
		h.Avg(), h.Pct(0.50), h.Pct(0.95), h.Pct(0.99), h.Max(), h.Count())
}
