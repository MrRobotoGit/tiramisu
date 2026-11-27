package app

import (
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"time"
	"tiramisu/internal/streampolicy"
	"tiramisu/internal/warmup"
)

// boostColdStart raises the torrent's connection limit at the first player read past the header,
// the same signal probePumpParked uses; header and Cues probes from scans never reach it.
func (h *MkvHandle) boostColdStart(off int64) {
	if h.hash == "" || off < streampolicy.HeaderProbeZone || off >= h.size-streampolicy.TailProbeZoneSize(h.size) || h.coldBoostTried.Swap(true) {
		return
	}
	eng.FriendsPlayback(h.hash, fmt.Sprintf("%p", h), "cold-start read", true)
	go func() {
		if t := eng.Get(h.hash); t != nil {
			t.BoostColdStart()
		}
	}()
}

// classifyHeaderRead applies streampolicy.IsHeaderRead and tracks the chain for the next read.
func (h *MkvHandle) classifyHeaderRead(prevOff, off int64, n int) bool {
	if !streampolicy.IsHeaderRead(prevOff, off, h.headerChainEnd.Load(), gc().SequentialTolerance) {
		h.headerChainEnd.Store(0)
		return false
	}
	h.headerChainEnd.Store(off + int64(n))
	return true
}

// storeReadOff records off as the handle's position unless it is a header read.
func (h *MkvHandle) storeReadOff(headerRead bool, off int64) {
	if headerRead {
		return
	}
	atomic.StoreInt64(&h.lastOff, off)
}

func (h *MkvHandle) promoteIfStreaming(prevOff, off int64, readLen int) {
	advance := prevOff >= 0 && streampolicy.AdvanceProvesStreaming(prevOff, off, int64(readLen), h.size, gc().SequentialTolerance, gc().StreamingThreshold)
	if !advance {
		h.seqRun.Store(0)
	} else if h.seqRun.Add(1) >= streampolicy.PrimaryProofReads {
		h.streamedOnce.Store(true)
	}
	if h.isPrimaryHandle.Load() || h.hash == "" || prevOff < 0 {
		return
	}
	if !advance {
		h.seqAdvances.Store(0)
		return
	}
	if h.seqAdvances.Add(1) < streampolicy.PrimaryProofFor(off, primaryOffsetFor(h.path, h), gc().ReadAheadBudget) {
		return
	}
	// Only during real playback: a background scan reads sequentially too, and outside a
	// confirmed session there is no player for the pump to follow anyway.
	val, ok := playbackRegistry.Load(h.path)
	if !ok {
		return
	}
	ps, ok := val.(*PlaybackState)
	if !ok || !(ps.GetStatus() || ps.IsInferredPlayback()) {
		return
	}
	// A second sequential reader (Plex intro or loudness analysis) must not take the pump from a
	// player that is watching: they swapped primary back and forth (fusesim analysis_same_file).
	if inc := activeStreamingPrimary(h.path, h); inc != nil {
		ps.mu.RLock()
		confirmedAt := ps.ConfirmedAt
		ps.mu.RUnlock()
		if !mayTakeOverStreamingPrimary(h, inc, confirmedAt) {
			return
		}
	}
	if becomePrimary(h) {
		logger.Printf("[V284] Handle promoted to primary after %d sequential reads at %dMB: %s",
			h.seqAdvances.Load(), off/(1024*1024), filepath.Base(h.path))
	}
}

// primaryOffsetFor returns the offset of the current primary handle on path, or 0 if there is none.
func primaryOffsetFor(path string, except *MkvHandle) int64 {
	var off int64
	activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != except && other.path == path && other.isPrimaryHandle.Load() {
			if o := atomic.LoadInt64(&other.lastOff); o > off {
				off = o
			}
		}
		return true
	})
	return off
}

// primaryActiveWindow is how recently a primary that has streamed must have read to count as a
// player that is watching.
const primaryActiveWindow = 2 * time.Second

// primaryStreamingFor reports a primary handle on path, other than except, that has streamed like
// a player and read within primaryActiveWindow. A stale probe primary (header and Cues only) never
// streamed, so it does not block the real player's promotion.
func primaryStreamingFor(path string, except *MkvHandle) bool {
	return activeStreamingPrimary(path, except) != nil
}

// mayTakeOverStreamingPrimary reports whether h may take primary from inc, a primary that is
// watching. Only the player of a session that started while inc was already reading qualifies:
// opened after inc, and before Plex confirmed the playback. An analysis started during a confirmed
// session opens after the confirmation (fusesim analysis_same_file); when the analysis was running
// first, the player opens after it and before the webhook (fusesim analysis_then_play).
func mayTakeOverStreamingPrimary(h, inc *MkvHandle, confirmedAt time.Time) bool {
	return !h.openedAt.IsZero() && h.openedAt.After(inc.openedAt) && !h.openedAt.After(confirmedAt)
}

// activeStreamingPrimary returns the primary handle on path, other than except, that has streamed
// like a player and read within primaryActiveWindow, or nil.
func activeStreamingPrimary(path string, except *MkvHandle) *MkvHandle {
	var found *MkvHandle
	now := time.Now()
	activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if !ok || other == except || other.path != path || !other.isPrimaryHandle.Load() || !other.streamedOnce.Load() {
			return true
		}
		other.mu.Lock()
		last := other.lastActivityTime
		other.mu.Unlock()
		if now.Sub(last) <= primaryActiveWindow {
			found = other
			return false
		}
		return true
	})
	return found
}

// becomePrimary makes h the only primary handle for its path, returning false if it already was.
// The pump follows the MAX of the primary offsets, so a probe that earned primary and then went
// idle kept dragging it to a frozen offset (Soulm8te, 2026-08-22: a stale PRI@8697MB anchored the
// pump 7.8GB past a player reading at 1120MB). Demotion is edge-triggered here at promotion time,
// never recomputed per pump tick: re-evaluating relevance every tick is what made the freshness
// heuristic in 6ae20c9 thrash (6517 jump pairs in 19h). Demoted handles lose their sequential
// streak too, so reclaiming primary costs a fresh streampolicy.PrimaryProofReads run and a frozen handle,
// which by definition stops reading, can never reclaim it at all.
func becomePrimary(h *MkvHandle) bool {
	if !h.isPrimaryHandle.CompareAndSwap(false, true) {
		return false
	}
	activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != h && other.path == h.path && other.isPrimaryHandle.CompareAndSwap(true, false) {
			other.seqAdvances.Store(0)
		}
		return true
	})
	return true
}

// seedResumeAnchor applies the tail-probe guard to a persisted resume position and,
// when valid, anchors the pump start there. ps is optional (nil on the DB fallback
// path). Returns true when the pump start was anchored. Zeroing a poisoned tail-probe
// value is a targeted UPDATE - it must never clobber the previous session's flags.
func (h *MkvHandle) seedResumeAnchor(ps *PlaybackState, lastSeekOff int64) bool {
	if lastSeekOff <= 2*1024*1024 {
		return false
	}
	anchor, zeroed := streampolicy.ResumeAnchorFromPersisted(lastSeekOff, h.size)
	if anchor > 0 {
		atomic.StoreInt64(&h.lastOff, anchor)
		logger.Printf("[ResumeAnchor] Seeding pump start from persisted resume position: %.1fMB",
			float64(anchor)/(1<<20))
		return true
	}
	if zeroed {
		// Self-heal: a value in the tail zone (or beyond EOF) is a poisoned tail probe,
		// never a real resume position (a genuine near-end resume is served by the SSD
		// tail warmup, see streampolicy.ShouldServeTailFromSSD, so it needs no pump anchor). Zero it
		// in memory + DB so it stops poisoning future pump starts.
		if ps != nil {
			ps.mu.Lock()
			ps.LastSeekOff = 0
			ps.mu.Unlock()
		}
		if stateDB != nil {
			if err := stateDB.UpdatePlaybackSeekOff(h.path, 0); err != nil {
				logger.Printf("[ResumeAnchor] Failed to zero poisoned last_seek_off for %s: %v", filepath.Base(h.path), err)
			} else {
				logger.Printf("[ResumeAnchor] Zeroed poisoned last_seek_off (%.1fMB in tail zone) for %s",
					float64(lastSeekOff)/(1<<20), filepath.Base(h.path))
			}
		}
	}
	return false
}

// forceTorrentWarmupActive marks hash/fileID as warmup-active unconditionally. Used at Open() for
// genuine cold starts (!headReady), before any WriteChunk has happened yet (and thus before
// DiskWarmup's writeWorker has had a chance to record the STARTING state itself), so aggressive
// PEX churn (Task 3) and the hedge watchdog (Task 4) catch the torrent's initial peer-connection
// burst instead of missing it. warmup.OnWarmupStateChange (wired in main() at startup) owns
// turning this back to false once the writeWorker actually observes STARTING/COMPLETED -
// synchronously, from inside processWrite itself, not by polling IsWarmingUp() from here (which
// would race ahead of the async write queue - WriteChunk only enqueues and returns immediately).
func forceTorrentWarmupActive(hash string, fileID int) {
	if warmup.DiskWarmup == nil {
		return
	}
	if t := eng.Get(hash); t != nil {
		t.SetWarmupActive(true, t.EngineFileIndex(fileID))
	}
}

// safeGo runs a function in a new goroutine with panic recovery.
// audioWakeGrace is how long Open waits for an audio activation before letting it run behind
// the pump: long enough to catch an immediate failure, short enough to keep the start fast.
const audioWakeGrace = 300 * time.Millisecond

func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[PANIC] Background goroutine recovered: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
