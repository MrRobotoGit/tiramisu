package app

import (
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"time"
	"tiramisu/internal/warmup"
)

// headerProbeZone is the start of the file that players and Samba re-read for container headers.
const headerProbeZone = 2 * 1024 * 1024

// boostColdStart raises the torrent's connection limit at the first player read past the header,
// the same signal probePumpParked uses; header and Cues probes from scans never reach it.
func (h *MkvHandle) boostColdStart(off int64) {
	if h.hash == "" || off < headerProbeZone || off >= h.size-tailProbeZoneSize(h.size) || h.coldBoostTried.Swap(true) {
		return
	}
	eng.FriendsPlayback(h.hash, fmt.Sprintf("%p", h), "cold-start read", true)
	go func() {
		if t := eng.Get(h.hash); t != nil {
			t.BoostColdStart()
		}
	}()
}

// headerChainLimit bounds how far a header read may continue sequentially and still count as one:
// the SSD warmup head (64MB by default). Plex's head reads ran past 16MB on every resume; inside
// the head the SSD serves the reads and the pump skips that zone anyway, so a player that really
// starts over loses nothing waiting until the head ends to reposition the pump.
func headerChainLimit() int64 {
	if warmup.FileSize > 16*1024*1024 {
		return warmup.FileSize
	}
	return 16 * 1024 * 1024
}

// isHeaderRead reports a header read by a handle already positioned past it, or a read continuing
// one sequentially up to headerChainLimit. It is not a seek (82% of seek interrupts in production
// landed here) and must not move the handle's offset, which the pump follows and Release persists
// as the resume position.
func isHeaderRead(prevOff, off, chainEnd, tolerance int64) bool {
	if prevOff < headerProbeZone {
		return false
	}
	if off < headerProbeZone {
		return true
	}
	limit := headerChainLimit()
	if off < limit && prevOff >= limit {
		// Plex parses the head out of order (2MB, then 28MB): any read inside the SSD head by a
		// handle positioned past it is still header parsing, sequential or not.
		return true
	}
	return chainEnd > 0 && off < limit && abs(off-chainEnd) <= tolerance
}

// classifyHeaderRead applies isHeaderRead and tracks the chain for the next read.
func (h *MkvHandle) classifyHeaderRead(prevOff, off int64, n int) bool {
	if !isHeaderRead(prevOff, off, h.headerChainEnd.Load(), gc().SequentialTolerance) {
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

// shouldInterruptForSeek returns true for genuine seeks beyond budget.
// Ignores: new handles (prevOff<=0), Samba header probes (off==0), sequential reads.
func shouldInterruptForSeek(prevOff, off, budget int64) bool {
	if prevOff <= 0 || off == 0 || (off < headerProbeZone && prevOff >= headerProbeZone) {
		return false
	}
	return off > prevOff+budget || prevOff > off+budget
}

// tailProbeZoneSize returns the adaptive tail-probe zone size for a file of the given size:
// 5%, clamped to [64MB, 2GB]. Shared by the LastSeekOff
// write-site guard, and the ResumeAnchor seed-site guard - a single scanner probe reading MKV
// Cues near the end of a file must never look like a real playback position to any of them.
// promoteIfStreaming marks a handle primary once it has proven it is the player: several
// consecutive sequential reads of streaming size. The pump syncs to primary handles only, and
// during steady playback the real reader is usually an attached (secondary) handle - the
// nominal primary can be an idle probe holding the first reference, so the pump follows the
// wrong offset and the player is served by FetchBlock (Soulm8te, 2026-08-19: 29 attaches, all
// primary=false, only a seek recovered it by forcing a refCount 0->1 reconnect).
//
// Promotion is monotonic and never recomputed per tick, unlike the freshness heuristic reverted
// in 6ae20c9: the pump takes the MAX of the primaries' offsets, so a promotion can only raise
// that value, never drag it backwards - which is what produced the jump/re-anchor thrashing.
// Tail-zone reads never count: an EOF probe must not be able to pull the pump to the end.
// advanceProvesStreaming reports whether one read looks like the player moving forward through
// the file: streaming-sized, sequential from the previous read, and outside the tail probe zone.
// The size floor is a quarter of StreamingThreshold so Synology's 64KB CIFS reads still count.
func advanceProvesStreaming(prevOff, off, readLen, size, tolerance, streamingThreshold int64) bool {
	if prevOff < 0 || off <= prevOff || readLen <= 0 {
		return false
	}
	if off-prevOff > readLen+tolerance {
		return false
	}
	if readLen < streamingThreshold/4 {
		return false
	}
	return off < size-tailProbeZoneSize(size)
}

const primaryProofReads = 3 // consecutive sequential reads before a handle drives the pump

func (h *MkvHandle) promoteIfStreaming(prevOff, off int64, readLen int) {
	advance := prevOff >= 0 && advanceProvesStreaming(prevOff, off, int64(readLen), h.size, gc().SequentialTolerance, gc().StreamingThreshold)
	if !advance {
		h.seqRun.Store(0)
	} else if h.seqRun.Add(1) >= primaryProofReads {
		h.streamedOnce.Store(true)
	}
	if h.isPrimaryHandle.Load() || h.hash == "" || prevOff < 0 {
		return
	}
	if !advance {
		h.seqAdvances.Store(0)
		return
	}
	if h.seqAdvances.Add(1) < primaryProofFor(off, primaryOffsetFor(h.path, h), gc().ReadAheadBudget) {
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

func tailProbeZoneSize(size int64) int64 {
	zone := size / 20 // 5%
	if zone < 64*1024*1024 {
		zone = 64 * 1024 * 1024
	}
	if zone > 2*1024*1024*1024 {
		zone = 2 * 1024 * 1024 * 1024
	}
	return zone
}

// shouldServeTailFromSSD reports whether a read at offset off (file size size) can be
// served from the tail warmup file (the last tailWarmupSize bytes). Pure gate for the
// Read() path: ReadTail itself is range-safe (returns 0 outside coverage), so this
// only decides whether the SSD tail is even a candidate. It deliberately does not
// depend on handle state - any first read with off >= warmup.FileSize transitions
// WARMUP→STREAMING (seek/resume detection) before a state-gated tail check could run.
// A resume in the last 16MB was therefore served by network FetchBlock (~16s TTFF on
// a 36.6GB remux, 2026-08-06) although the tail file was complete on SSD.
func shouldServeTailFromSSD(off, size, tailWarmupSize int64) bool {
	if size <= 0 || tailWarmupSize <= 0 {
		return false
	}
	tailStart := size - tailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	return off >= tailStart
}

// shouldRecordResumeOff reports whether a read may update the persisted resume position.
// PlaybackState is per-path, so before the single-primary invariant any handle touching the file
// could write here, and the monotonic guard it used to carry made the value a high-water mark
// that never came back down: a probe reading at 6431MB became the "resume position" for a player
// sitting at 1300MB, and the next session anchored the pump 5GB off (Lucky S01E04, 2026-08-22).
// Gating on the primary handle and dropping the monotonic guard records where the player
// actually is. The 2MB floor and the tail-probe ceiling still exclude header and Cues reads.
func shouldRecordResumeOff(healthy, isPrimary bool, off, size int64) bool {
	if !healthy || !isPrimary || size <= 0 {
		return false
	}
	return off > 2*1024*1024 && off < size-tailProbeZoneSize(size)
}

// resumeAnchorFromPersisted applies the tail-probe guard to a persisted resume
// position. Returns (anchor, zeroed): anchor is a valid pump start (0 when unusable),
// zeroed=true when the value lies in the tail-probe zone (or beyond EOF) and must be
// cleared - a single scanner probe reading MKV Cues near the end of a file must never
// look like a real playback position. Shared by the registry seeding and the DB
// fallback so both paths apply identical semantics.
func resumeAnchorFromPersisted(lastSeekOff, size int64) (anchor int64, zeroed bool) {
	if lastSeekOff > 2*1024*1024 && lastSeekOff < size-tailProbeZoneSize(size) {
		return lastSeekOff, false
	}
	if lastSeekOff >= size-tailProbeZoneSize(size) {
		return 0, true
	}
	return 0, false
}

// shouldInterruptPump decides whether a read should interrupt the pump. Reads served
// from the SSD tail warmup (shouldServeTailFromSSD) never touch the pump - Interrupt()
// + ResetShield() would only stall a pump the read does not need. The old isTailProbe
// gate was unreachable dead state (any read in the tail zone first transitions
// WARMUP→STREAMING), so a live scrub into the last 16MB interrupted the pump even
// though the SSD served the read instantly. Leaving the SSD tail is no seek either: Plex reads
// the Cues there and then the head, which interrupted the pump and reset the global shield.
func shouldInterruptPump(prevOff, off, budget int64, tailServed, prevTailServed bool) bool {
	if tailServed || prevTailServed {
		return false
	}
	return shouldInterruptForSeek(prevOff, off, budget)
}

// primaryProofFor returns how many consecutive sequential reads a challenger must show before it
// may take primary. One landing within the read-ahead budget of the incumbent is the player moving
// normally (its own reopen lands right where it left off) and needs the base proof. One landing
// further away would drag the pump across the file, so it must sustain the streak far longer than
// a short-lived probe can: Lucky S01E04, 2026-08-22 saw handles attach at 5578/6069/6430MB, earn
// primary within 2-3s and be released immediately, each yanking the pump away from a player
// reading 2GB behind. A genuine seek by an open handle promotes through Read()'s seek path
// instead, so it never pays this. The extended proof is also the escape hatch that stops a wrong
// incumbent from wedging primary forever.
func primaryProofFor(challengerOff, incumbentOff, budget int64) int32 {
	if incumbentOff <= 0 || budget <= 0 {
		return primaryProofReads // no incumbent to displace
	}
	d := challengerOff - incumbentOff
	if d < 0 {
		d = -d
	}
	if d <= budget {
		return primaryProofReads
	}
	return primaryProofReads * 4
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
// streak too, so reclaiming primary costs a fresh primaryProofReads run and a frozen handle,
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

// pumpStrandedAhead reports whether the pump sits so far past the player that it can no longer
// serve it. nativePumpChunk's hard limit already parks a healthy pump at ~budget, so only a lead
// beyond 2x budget means a bad anchor rather than throttling. playerOff == 0 means no primary
// handle is registered right now (handle handoff), and must never drag the pump back to 0.
func pumpStrandedAhead(offset, playerOff, budget int64) bool {
	if playerOff <= 0 || budget <= 0 {
		return false
	}
	return offset > playerOff+2*budget
}

// shouldFireStarvationRevival decides whether the pump loop must self-revive: the
// pump is confirmed playback, its lead over the player is thin (< 16MB), and it is
// advancing below 75% of the player's consumption rate for the observation window.
// This replicates what a manual player seek does via V286b (Interrupt + ResetShield +
// fresh peer selection) without moving the player. Rates are bytes/second averaged
// over the 10s window by the caller. The lead gate protects the healthy hard-limit
// idle (lead = ReadAheadBudget): a pump with a thick lead is deliberately stopped,
// not starved (coincidentally the same 16MB as warmup.TailWarmupSize, but a
// different purpose: the watchdog lead, not the SSD tail coverage). Cooldown
// prevents revival storms.
func shouldFireStarvationRevival(confirmed bool, leadBytes int64, pumpRate, playerRate float64, cooldownAge time.Duration) bool {
	if !confirmed || cooldownAge < 60*time.Second {
		return false
	}
	if playerRate <= 0 || leadBytes >= 16*1024*1024 {
		return false
	}
	return pumpRate < playerRate*0.75
}

// seedResumeAnchor applies the tail-probe guard to a persisted resume position and,
// when valid, anchors the pump start there. ps is optional (nil on the DB fallback
// path). Returns true when the pump start was anchored. Zeroing a poisoned tail-probe
// value is a targeted UPDATE - it must never clobber the previous session's flags.
func (h *MkvHandle) seedResumeAnchor(ps *PlaybackState, lastSeekOff int64) bool {
	if lastSeekOff <= 2*1024*1024 {
		return false
	}
	anchor, zeroed := resumeAnchorFromPersisted(lastSeekOff, h.size)
	if anchor > 0 {
		atomic.StoreInt64(&h.lastOff, anchor)
		logger.Printf("[ResumeAnchor] Seeding pump start from persisted resume position: %.1fMB",
			float64(anchor)/(1<<20))
		return true
	}
	if zeroed {
		// Self-heal: a value in the tail zone (or beyond EOF) is a poisoned tail probe,
		// never a real resume position (a genuine near-end resume is served by the SSD
		// tail warmup, see shouldServeTailFromSSD, so it needs no pump anchor). Zero it
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
