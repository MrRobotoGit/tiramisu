package app

import (
	"context"
	_ "net/http/pprof"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"tiramisu/internal/bridge"
	"tiramisu/internal/engine"
	"tiramisu/internal/mkvinfo"
	"tiramisu/internal/piecemap"
	"tiramisu/internal/streampolicy"
	"tiramisu/internal/warmup"
)

// NativePumpState tracks a shared pump across multiple handles for the same file.
type NativePumpState struct {
	cancel           context.CancelFunc
	reader           *bridge.NativeReader
	path             string
	refCount         int32
	playerOff        int64       // last known player position, saved on handle release
	interruptPending atomic.Bool // prevents cascade: only the first handle per seek fires Interrupt()
}

// attachToExistingPump links the handle to a running pump exactly once. The caller holds
// pumpCreationMu, which serializes attaches on this path, and the hasSlot re-check under
// h.mu keeps the state assignment and the refCount increment together: two reads racing on
// the same handle would otherwise both attach, and Release could never bring refCount back
// to zero, pinning the pump and its slot until the reader idle timeout.
func (h *MkvHandle) attachToExistingPump(ps *NativePumpState) bool {
	h.mu.Lock()
	if h.hasSlot.Load() {
		h.mu.Unlock()
		return false
	}
	h.hasSlot.Store(true)
	h.pumpState = ps
	h.isWatching = true
	h.nativeReader = ps.reader
	h.pumpCancel = ps.cancel
	h.mu.Unlock()
	atomic.AddInt32(&ps.refCount, 1)
	globalOpenTracker.IncFor(h, h.hash, h.path)
	return true
}

// announceForNotice forces a tracker announce for a swarm with no peer: the peers it returned and
// the trackers that answered without error.
func announceForNotice(hash string, done func(peers, answered int)) {
	if t := eng.Get(hash); t != nil {
		t.AnnounceTracked(func(peers, errs, total int, _ []string) { done(peers, total-errs) })
	}
}

// terminateOrphanPump cancels and forgets any pump registered for path. The cleanup sweep
// uses it when pruning a zombie registry entry: without it the pump keeps its slot and keeps
// fetching until the reader idle timeout (V262, up to 2h with an open handle).
func terminateOrphanPump(path string) bool {
	val, ok := activePumps.Load(path)
	if !ok {
		return false
	}
	if ps, ok := val.(*NativePumpState); ok && ps.cancel != nil {
		ps.cancel()
	}
	activePumps.Delete(path)
	return true
}

// startNativePump acquires a slot and starts the background pump.
// Called from Open (proactive) or Read (rescue for late resolution).
func (h *MkvHandle) startNativePump(finalHash string, fileIdx int) {
	// 1. Verify we don't already have a slot or an active pump
	if h.hasSlot.Load() {
		return
	}

	isHealthy := false
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
			isHealthy = true
		}
	}

	// StrategicReserve: limit background scan slots.
	// If any confirmed playback is active (IsHealthy), tighten the limit to 5 slots
	// so the scan cannot consume memory that the active stream needs.
	// Without active playback, allow up to MasterConcurrencyLimit-5 (default 20).
	canTakeSlot := true
	if !isHealthy {
		// capacity is the semaphore's, fixed at make(): the live config would apply half a change.
		anyHealthyPlayback := anyConfirmedPlayback()
		scanLimit := engine.ScanSlotLimit(cap(masterDataSemaphore), anyHealthyPlayback)
		if len(masterDataSemaphore) >= scanLimit {
			canTakeSlot = false
			logger.Printf("[StrategicReserve] Denying pump slot to background scan (Saturation: %d/%d, healthyPlayback=%v): %s",
				len(masterDataSemaphore), gc().MasterConcurrencyLimit, anyHealthyPlayback, filepath.Base(h.path))
		}
	}

	if !canTakeSlot {
		return
	}

	pumpCreationMu.Lock()

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		newRefs := atomic.AddInt32(&ps.refCount, 1)
		h.mu.Lock()
		h.hasSlot.Store(true)
		h.pumpState = ps
		h.isWatching = true
		h.nativeReader = ps.reader
		h.pumpCancel = ps.cancel
		h.mu.Unlock()
		globalOpenTracker.IncFor(h, h.hash, h.path)
		// Primary reconnect (refCount 0→1): inherit player position.
		// Secondary handles (Plex probes at arbitrary offsets): no inheritance.
		if newRefs == 1 {
			becomePrimary(h)
			if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
				atomic.StoreInt64(&h.lastOff, curPos)
			}
		}
		logger.Printf("[V264] Attached to existing pump (Refs: %d, primary=%v): %s", newRefs, h.isPrimaryHandle.Load(), filepath.Base(h.path))
		pumpCreationMu.Unlock()
		return
	}

	// Release mutex before blocking on semaphore to avoid holding it during I/O.
	pumpCreationMu.Unlock()

	select {
	case masterDataSemaphore <- struct{}{}:
		// Double-check activePumps after acquiring semaphore (another goroutine may have created it).
		pumpCreationMu.Lock()
		if val, ok := activePumps.Load(h.path); ok {
			<-masterDataSemaphore
			ps := val.(*NativePumpState)
			newRefs := atomic.AddInt32(&ps.refCount, 1)
			h.mu.Lock()
			h.hasSlot.Store(true)
			h.pumpState = ps
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			globalOpenTracker.IncFor(h, h.hash, h.path)
			if newRefs == 1 {
				becomePrimary(h)
				if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
					atomic.StoreInt64(&h.lastOff, curPos)
				}
			}
			pumpCreationMu.Unlock()
			return
		}

		h.hasSlot.Store(true)
		becomePrimary(h) // pump creator is always primary
		// Capture the reader now and pass it to the pump goroutine: re-reading h.nativeReader
		// inside the goroutine raced with Release setting it to nil, and the resulting early
		// return (before the teardown defer) leaked the master slot.
		pumpReader := nativeBridge.NewStreamReader(finalHash, fileIdx, h.size)
		h.nativeReader = pumpReader
		if t := eng.Get(finalHash); t != nil {
			if ts := t.Store(); ts != nil && ts.PieceLen() > 0 {
				pl := ts.PieceLen()
				pumpReader.SetPieceLen(pl)
				raCache.SetPieceLen(h.path, pl)
			}
		}

		// Register in activePumps BEFORE releasing lock, but BEFORE doing I/O
		pumpCtx, pumpCancel := context.WithCancel(context.Background())
		h.pumpCancel = pumpCancel
		sharedState := &NativePumpState{
			cancel:   pumpCancel,
			reader:   pumpReader,
			path:     h.path,
			refCount: 1,
		}
		activePumps.Store(h.path, sharedState)
		h.pumpState = sharedState
		globalOpenTracker.IncFor(h, h.hash, h.path)
		pumpCreationMu.Unlock() // SUCCESS: Shared state registered, global lock released

		logger.Printf("[V264] Native Pump Started (Slot Acquired): %s", filepath.Base(h.path))

		// Seed h.lastOff from the persisted resume position (V750 playback_states.last_seek_off)
		// before any of the anchoring logic below runs, so a cold pump-start (h.lastOff still -1,
		// no live signal yet) for a file resumed deep into its runtime is treated exactly like an
		// inherited primary-reconnect position (see newRefs==1 above) instead of starting near
		// MaxCachedOffset/0 - without this, resuming at e.g. 19GB left the pump buffering from
		// scratch while the player was served entirely by FetchBlock until something else caught
		// up. >2MB matches the existing real-playback-vs-scanner threshold (PlaybackState.IsInferredPlayback).
		if atomic.LoadInt64(&h.lastOff) < 0 {
			seeded := false
			if val, ok := playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok {
					pbs.mu.RLock()
					lastSeekOff := pbs.LastSeekOff
					pbs.mu.RUnlock()
					seeded = h.seedResumeAnchor(pbs, lastSeekOff)
				}
			}
			// V751: registry miss or fresh entry (a clean stop before boot leaves
			// IsStopped=true, which restorePlaybackStates filters out) → fall back to the
			// DB so a mid-file resume still anchors the pump at the real position instead
			// of starting from 0 while the player is served entirely by FetchBlock.
			if !seeded && stateDB != nil {
				if rec, err := stateDB.LoadPlaybackStateByPath(h.path); err == nil && rec != nil && rec.LastSeekOff > 0 {
					if h.seedResumeAnchor(nil, rec.LastSeekOff) {
						logger.Printf("[ResumeAnchor] Seeded pump start from DB: %.1fMB", float64(rec.LastSeekOff)/(1<<20))
					}
				}
			}
		}

		// Start background pump — resume from last cached position
		resumeOffset := raCache.MaxCachedOffset(h.path)

		// Start pump near end of warmup zone so it buffers past 64MB before SSD handover.
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
			diskOffset := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			if diskOffset > 16*1024*1024 {
				safetyMargin := int64(16 * 1024 * 1024)
				skipOffset := diskOffset - safetyMargin
				if skipOffset > resumeOffset {
					resumeOffset = skipOffset
					logger.Printf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB to bridge SSD handover", float64(resumeOffset)/(1<<20))
				}
			}
		}

		// Anchor pump to player position when MaxCachedOffset is stale-high to prevent EOF loops.
		if playerOff := atomic.LoadInt64(&h.lastOff); playerOff > 0 {
			chunkSize := raCache.ChunkSize(h.path)
			if resumeOffset > playerOff+chunkSize*2 {
				aligned := (playerOff / chunkSize) * chunkSize
				logger.Printf("[V700] Pump anchored to player: %.1fMB (MaxCached was %.1fMB)",
					float64(aligned)/(1<<20), float64(resumeOffset)/(1<<20))
				resumeOffset = aligned
			}
		} else if playerOff < 0 && resumeOffset > 0 {
			// New handle: reset stale MaxCachedOffset unless warmup is active and covers the range.
			// If resumeOffset >= warmup.FileSize, pump skip cannot fire → dead zone in raCache.
			warmupCoverage := int64(0)
			if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
				warmupCoverage = warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			}
			if warmupCoverage == 0 || resumeOffset >= warmup.FileSize {
				logger.Printf("[V700] New handle: reset stale MaxCachedOffset %.1fMB → 0",
					float64(resumeOffset)/(1<<20))
				resumeOffset = 0
			}
		}

		// Anchor pump to player position on resume to eliminate anacrolix priority competition.
		{
			chunkSize := raCache.ChunkSize(h.path)
			if raV310 := atomic.LoadInt64(&h.lastOff); raV310 > 0 && resumeOffset+chunkSize < raV310 {
				pumpStartV310 := (raV310 / chunkSize) * chunkSize
				logger.Printf("[V310] Resume anchor: pump start → %dMB (player at %dMB)",
					pumpStartV310/(1024*1024), raV310/(1024*1024))
				resumeOffset = pumpStartV310
			}
		}

		if resumeOffset > 0 {
			atomic.StoreInt64(&h.lastOff, resumeOffset)
		}

		pumpStart := resumeOffset
		capturedState := sharedState
		safeGo(func() {
			h.nativePump(pumpCtx, pumpReader, pumpStart, capturedState)
		})
	default:
		// If slots are full, it will fall back to per-request slots in Read
		logger.Printf("[MasterSemaphore] Limit reached, %s will use Fallback mode", filepath.Base(h.path))
	}
}

// nativePump reads continuously from the Native pipe and fills raCache.
// sharedState guards against orphan-delete in defer. pumpReader is captured by the caller
// so a concurrent Release cannot nil it out from under the goroutine.
func (h *MkvHandle) nativePump(ctx context.Context, pumpReader *bridge.NativeReader, startOffset int64, sharedState *NativePumpState) {
	// The master slot must be returned on every exit, including the early return below.
	// It used to be released only by the teardown defer, which the nil-reader return path
	// never reached, leaking one slot per event.
	defer func() {
		if h.hasSlot.Load() {
			select {
			case <-masterDataSemaphore:
			default:
			}
			h.hasSlot.Store(false)
		}
	}()

	if pumpReader == nil {
		logger.Printf("[Pump] reader is nil at startup for %s", filepath.Base(h.path))
		return
	}
	// ctx is checked only at the top of the loop; without this a cancelled pump blocked in
	// ReadAt keeps its slot and keeps downloading until the stream timeout (seen up to 90s).
	stopInterruptOnCancel := context.AfterFunc(ctx, pumpReader.Interrupt)
	defer stopInterruptOnCancel()

	if h.hash == "" {
		// Late hash resolution for handles where Open() didn't complete it.
		if hash, fileID, err := resolveTargetFileFn(h.url, h.size, h.path); err == nil {
			h.mu.Lock()
			h.hash = hash
			h.fileID = fileID
			h.mu.Unlock()
			logger.Printf("[Pump] Late resolution success: %s", hash[:8])
		} else {
			logger.Printf("[Pump] Warning: hash empty for %s, warmup disabled", filepath.Base(h.path))
		}
	}
	// reachedEOF separates the normal end (the whole file is fetched) from an early exit:
	// V306 used to warn on both, and 263 of 332 historical warnings were plain EOF.
	reachedEOF := false
	defer func() {
		// V306: an early exit during confirmed playback is unexpected; reaching EOF is not.
		pumpExitedHealthy := false
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
				pumpExitedHealthy = true
			}
		}

		h.mu.Lock()
		// Only delete if our sharedState is still the registered one (prevents pump A's defer from deleting pump B).
		if val, ok := activePumps.Load(h.path); ok && val == sharedState {
			activePumps.Delete(h.path)
			// Purge the adaptive chunk size set at this pump's start (SetPieceLen, in startNativePump):
			// otherwise pieceLens keeps growing with entries for paths no pump is streaming
			// anymore, and a future pump for the same path with a different piece length would
			// see the stale entry via ChunkSize() until a fresh SetPieceLen call overwrites it.
			raCache.ForgetPieceLen(h.path)
		}

		if pumpExitedHealthy && reachedEOF {
			logger.Printf("[V306] Pump reached end of file during playback for %s: download complete", filepath.Base(h.path))
		} else if pumpExitedHealthy {
			logger.Printf("[V306] WARNING: pump goroutine exited during healthy playback for %s — priority preserved via IsHealthy", filepath.Base(h.path))
		}

		// The master slot is released by the defer installed at the top of nativePump.
		pumpReader.Close()
		h.mu.Unlock()
		logger.Printf("[V239] Native Pump Goroutine Ended: %s", filepath.Base(h.path))
	}()

	// V752: starvation watchdog state — sliding 10s window + last revival time.
	var watchSamples []watchSample
	lastPlayerOff := int64(0) // kept while the primary only reads the SSD tail
	lastRevival := time.Now().Add(-time.Hour)

	chunkSize := raCache.ChunkSize(h.path)

	// Track bytes pumped in this session for the Grace Period Boost
	pumpedBytes := int64(0)
	// Align startOffset to chunk boundary
	offset := (startOffset / chunkSize) * chunkSize

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-read chunkSize each iteration for consistency with raCache's own lookups - not
		// because it can change mid-pump. SetPieceLen is called exactly once, at pump start
		// (in startNativePump), only if metadata is already resolved at that moment; if not, this
		// stays at the ReadAheadBase fallback for the pump's entire lifetime. No late-binding path
		// exists today - if one is ever added, chunkKey's offset/chunkSize indexing means a
		// mid-pump size change must purge that path's existing raCache entries first, or stale
		// keys orphan and double-cache.
		chunkSize = raCache.ChunkSize(h.path)

		// Release idle slot: confirmed playback gets 2h, background scans get 45s.
		// Check all handles for this path, not just the pump creator.
		lastAct := time.Time{}
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				handle.mu.Lock()
				if handle.lastActivityTime.After(lastAct) {
					lastAct = handle.lastActivityTime
				}
				handle.mu.Unlock()
			}
			return true
		})
		// Fallback to pump creator's activity if no active handles found
		if lastAct.IsZero() {
			h.mu.Lock()
			lastAct = h.lastActivityTime
			h.mu.Unlock()
		}

		timeoutLimit := 45 * time.Second
		playbackConfirmedOrInferred := false
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok {
				if ps.GetStatus() {
					timeoutLimit = 2 * time.Hour
					playbackConfirmedOrInferred = true
				} else if ps.IsInferredPlayback() {
					timeoutLimit = 10 * time.Minute // V750: Inferred playback
					playbackConfirmedOrInferred = true
				}
			}
		}

		if time.Since(lastAct) > timeoutLimit {
			logger.Printf("[V262] Idle timeout (%v) for %s - yielding slot", timeoutLimit, filepath.Base(h.path))
			return
		}

		// Sync to primary handles only; secondary metadata probes cause false 10GB+ jumps.
		now := time.Now()
		playerOff := int64(0)
		tailOnly := false
		anyReaderOff := int64(0) // any handle, tail probes excluded: before confirmation the player can be a secondary
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				off := atomic.LoadInt64(&handle.lastOff)
				// SSD-tail reads are Cues probes served from disk: following them sent the pump to EOF.
				if handle.isPrimaryHandle.Load() {
					if streampolicy.ShouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize) {
						tailOnly = true
					} else if off > playerOff {
						playerOff = off
					}
				}
				if off > anyReaderOff && off < h.size-streampolicy.TailProbeZoneSize(h.size) {
					anyReaderOff = off
				}
			}
			return true
		})
		if playerOff == 0 && tailOnly {
			playerOff = lastPlayerOff
		}
		lastPlayerOff = playerOff

		// Snap pump to player position when seek gap exceeds budget, aligned to chunk boundary.
		jumpThreshold := int64(gc().ReadAheadBudget)
		if playerOff > offset+jumpThreshold {
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Printf("[V284] Pump jump: %dMB → %dMB (player at %dMB, gap %dMB)",
				offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024),
				(playerOff-offset)/(1024*1024))
			offset = jumpTo
			pumpedBytes = 0                 // reset grace period so throttle doesn't fire immediately
			watchSamples = watchSamples[:0] // V752: fresh window after jump — rates across a jump are meaningless
		} else if streampolicy.PumpStrandedAhead(offset, playerOff, jumpThreshold) {
			// V754: the snap above only fires forward, so a pump anchored past the player could
			// never rejoin it — nativePumpChunk's hard limit parked it in a 100ms sleep for the
			// whole session while every read fell through to blocking FetchBlock.
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Printf("[V754] Pump re-anchor (stranded ahead): %dMB → %dMB (player at %dMB, lead %dMB)",
				offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024),
				(offset-playerOff)/(1024*1024))
			offset = jumpTo
			pumpedBytes = 0
			watchSamples = watchSamples[:0]
		}

		// V752: starvation watchdog — revive a pump that crawls below the player's
		// consumption rate while its lead is thin, without waiting for a player seek.
		// A blocked read inside nativePumpChunk cannot evaluate here; fetch timeouts
		// (p95/hedge scale) return control to the loop and the next iteration fires.
		watchSamples = append(watchSamples, watchSample{t: now, offset: offset, playerOff: playerOff})
		if len(watchSamples) > 16 {
			watchSamples = watchSamples[len(watchSamples)-16:] // V752: size-bounded ring — sparse crawls must survive in the window
		}
		if len(watchSamples) >= 2 {
			first, lastS := watchSamples[0], watchSamples[len(watchSamples)-1]
			elapsed := lastS.t.Sub(first.t).Seconds()
			if elapsed >= 9 {
				pumpRate := float64(lastS.offset-first.offset) / elapsed
				playerRate := float64(lastS.playerOff-first.playerOff) / elapsed
				if streampolicy.ShouldFireStarvationRevival(playbackConfirmedOrInferred, offset-playerOff, pumpRate, playerRate, now.Sub(lastRevival)) {
					// Same core as V286b: Interrupt unblocks the in-flight chunk read and
					// the retry path re-issues it with fresh peer selection; ResetShield
					// clears adaptive state. interruptPending prevents cascade with seeks.
					if ps := sharedState; !ps.interruptPending.Swap(true) {
						// pumpReader, not h.nativeReader: Release nils the field while the pump
						// keeps its captured copy alive, so the field can be nil right here.
						pumpReader.Interrupt()
						eng.ResetShield(h.hash)
						lastRevival = now
						watchSamples = watchSamples[:0]
						logger.Printf("[StarvationWatchdog] Revival: pump %dMB vs player %dMB (rates %.1f/%.1f MB/s) — interrupt+shield reset",
							offset/(1<<20), playerOff/(1<<20), pumpRate/(1<<20), playerRate/(1<<20))
					}
				}
			}
		}

		// Throttle background pump after 64MB grace period.
		if pumpedBytes > 64*1024*1024 {
			isHealthy := false
			if val, ok := playbackRegistry.Load(h.path); ok {
				if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
					isHealthy = true
				}
			}

			if !isHealthy {
				// Throttle background tasks: 150ms delay between 16MB chunks.
				time.Sleep(150 * time.Millisecond)
			}
		}

		if probePumpParked(playbackConfirmedOrInferred, max(playerOff, anyReaderOff), offset, chunkSize) {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		stop, nextOffset := h.nativePumpChunk(pumpReader, offset, chunkSize, playerOff)
		if stop {
			if ctx.Err() != nil {
				return
			}
			// Transient errors (seek interrupt, reconnect, piece timeout): retry until genuine EOF.
			if offset < h.size {
				time.Sleep(200 * time.Millisecond)
				// V286b: reset after each interrupt so subsequent seeks can fire Interrupt()
				// and ResetShield() again. Without this, interruptPending stays true for the
				// pump's entire lifetime after the first seek.
				sharedState.interruptPending.Store(false)
				continue
			}
			reachedEOF = true
			return // genuine EOF
		}

		pumpedBytes += (nextOffset - offset)
		offset = nextOffset
	}
}

// probePumpParked reports whether a pump with no evidence of playback has filled enough. Every
// Open starts a pump, and a probe that read only the header left it free to fetch the full lead
// (~200MB) into nothing. Until playback is confirmed/inferred or any handle reads past the header
// (tail probes excluded; before confirmation the player is often an attached secondary), the pump
// fills the warmup head plus one chunk; a real player crosses 2MB within a second.
func probePumpParked(confirmedOrInferred bool, playerOff, offset, chunkSize int64) bool {
	if confirmedOrInferred || playerOff >= streampolicy.HeaderProbeZone {
		return false
	}
	return offset >= warmup.FileSize+chunkSize
}

// nativePumpChunk reads a single chunk from the Native pipe into raCache.
// filmHeaderProbe is how much of the file head the MKV Info lookup reads.
const filmHeaderProbe = 256 << 10

// filmBitrateOf reads the film's duration from the SSD head or the read-ahead cache (a var: the
// FUSE simulator serves no real MKV). An MP4 named .mkv keeps it in its moov, in the head or at
// the end of the file.
var filmBitrateOf = func(h *MkvHandle) (int64, bool) {
	head := make([]byte, filmHeaderProbe)
	n := 0
	warm := warmup.DiskWarmup != nil && h.usesWarmup && h.hash != ""
	if warm {
		n, _ = warmup.DiskWarmup.ReadAt(h.hash, h.fileID, head, 0)
	}
	if n < len(head) {
		n = max(n, raCache.CopyTo(h.path, 0, int64(len(head))-1, head))
	}
	if b, ok := mkvinfo.Bitrate(head[:n], h.size); ok || n < 8 || string(head[4:8]) != "ftyp" {
		return b, ok
	}
	// MP4: the moov sits in a larger head (fast-start) or at the end of the file. Each is
	// searched once, when its 16 MB are on the SSD; an MP4 without a duration stays unknown.
	if !warm {
		return 0, false
	}
	span := min(warmup.TailWarmupSize, h.size)
	if warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID) >= span && h.mp4HeadTried.CompareAndSwap(false, true) {
		big := make([]byte, span)
		nb, _ := warmup.DiskWarmup.ReadAt(h.hash, h.fileID, big, 0)
		if b, ok := mkvinfo.BitrateOf(big[:nb], nil, h.size); ok {
			return b, ok
		}
	}
	if warmup.DiskWarmup.TailReady(h.hash, h.fileID) && h.mp4TailTried.CompareAndSwap(false, true) {
		tail := make([]byte, span)
		m, _ := warmup.DiskWarmup.ReadTail(h.hash, h.fileID, tail, h.size-span, h.size)
		return mkvinfo.BitrateOf(nil, tail[:m], h.size)
	}
	return 0, false
}

// applyFilmBitrate gives the pump's stream the film's bitrate (size / MKV duration), from the
// SSD head or the read-ahead cache, retried every few seconds until the header is local.
func (h *MkvHandle) applyFilmBitrate(r *bridge.NativeReader) {
	if h.bitrateKnown.Load() || !strings.HasSuffix(h.path, ".mkv") {
		return
	}
	now := time.Now().UnixNano()
	if last := h.bitrateTried.Load(); now-last < int64(5*time.Second) || !h.bitrateTried.CompareAndSwap(last, now) {
		return
	}
	b, ok := filmBitrateOf(h)
	if !ok {
		return
	}
	r.SetBitrate(b)
	h.bitrateKnown.Store(true)
	logger.Printf("[Streamer] Film bitrate %.1f MB/s for %s", float64(b)/(1<<20), filepath.Base(h.path))
}

// pumpLeadLimit caps the pump's lead so the player stays inside the raCache window. The cache
// holds the last `budget` bytes the pump wrote, i.e. [pumpOff-budget, pumpOff]; the pump stops
// only once diff EXCEEDS the limit, so its settled lead is limit+chunkSize. With limit == budget
// the player sits a whole chunk below the window and every read on that edge misses (Lucky
// S01E04, 2026-08-22: pump-off pinned at 272MB against a 256MB cache, 40 consecutive stalls).
// Reserving three chunks leaves two of margin after the overshoot. Budget and chunkSize are both
// user-configurable, so the margin is derived, never hardcoded; a budget too small to hold it
// keeps the old behaviour rather than starving the lead.
func pumpLeadLimit(budget, chunkSize int64) int64 {
	if budget <= 0 || chunkSize <= 0 {
		return budget
	}
	slack := 3 * chunkSize
	if slack > budget/2 {
		slack = budget / 2
	}
	lead := budget - slack
	if lead < chunkSize {
		lead = chunkSize
	}
	if lead > budget {
		lead = budget
	}
	return lead
}

func (h *MkvHandle) nativePumpChunk(r *bridge.NativeReader, offset, chunkSize, playerOff int64) (stop bool, nextOffset int64) {
	// Don't pump beyond file size
	if offset >= h.size {
		return true, offset
	}

	budget := gc().ReadAheadBudget
	leadLimit := pumpLeadLimit(budget, chunkSize)
	diff := offset - playerOff

	if diff > leadLimit {
		// Hard limit reached: Wait for player to advance.
		time.Sleep(100 * time.Millisecond)
		return false, offset
	} else if diff > (leadLimit * 7 / 10) {
		// Soft limit (70%): Slow down gradually.
		sleepMs := (diff - (leadLimit * 7 / 10)) / (1024 * 1024)
		if sleepMs > 50 {
			sleepMs = 50
		}
		if sleepMs > 0 {
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}

	// Playback pressure: the pump's lead over the player has worn thin (opposite end of the same
	// diff/budget signal used to throttle above). Extends tail-hedging into steady-state playback
	// via the same mechanism already used for warmup - see SetPlaybackPressure. Called on every
	// chunk like SetWarmupActive already is; both just store atomics, cheap either way.
	if h.hash != "" {
		if t := eng.Get(h.hash); t != nil {
			t.SetPlaybackPressure(diff < budget*20/100, offset)
		}
	}
	r.SetPlayerLead(diff) // the streamer's stall risk counts what the pump already holds
	h.applyFilmBitrate(r)

	end := offset + chunkSize
	if end > h.size {
		end = h.size
	}

	// Coverage-aware skip-check: must cover the WHOLE [offset, end-1] range, not just probe the
	// first byte. A single-byte probe would still hit after a partial Put (see the drift fix
	// below), letting the pump skip a chunk that's only partially cached and leave a hole.
	// Covered() does the check without copying; this is the common path in steady-state
	// playback, so only pay for Get()'s up-to-16MB defensive copy when DiskWarmup actually
	// needs the bytes.
	if raCache.Covered(h.path, offset, end-1) {
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			if data := raCache.Get(h.path, offset, end-1); data != nil {
				warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, offset)
			}
		}
		return false, offset + chunkSize
	}

	// Skip warmup zone during initial play (SSD serves 0-80MB); pump jumps ahead to pre-fill raCache.
	// Gated on stateWarmup to avoid skip on resume/seek.
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if warmupCoverage >= offset+chunkSize {
			return false, offset + chunkSize
		}
	}

	// Use buffer from pool to reduce allocations
	bufPtr := readBufferPool.Get().(*[]byte)
	defer readBufferPool.Put(bufPtr)

	n, err := r.ReadAt((*bufPtr)[:end-offset], offset)
	if n > 0 {
		raCache.Put(h.path, offset, offset+int64(n)-1, (*bufPtr)[:n])
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, (*bufPtr)[:n], offset)
		}
	}

	if err != nil {
		return true, offset + int64(n)
	}

	// A short read (fewer bytes than requested, err==nil) happens when a stream hiccup gets
	// silently converted to (n, nil) upstream (see native.go ReadAt's ErrUnexpectedEOF handling).
	// Continuing from offset+n would leave the pump mid-chunk permanently: every Put/skip-check
	// keys off ChunkSize-aligned offsets, so an unaligned continuation drifts off the chunk grid
	// for the rest of the pump's lifetime, turning every future chunk boundary into a cache miss.
	// Snap forward to the next chunk boundary instead; the small gap this skips is filled on demand
	// by FUSE reads landing in it (native.go's ReadAt handles both small and large forward jumps).
	got := offset + int64(n)
	if int64(n) < end-offset {
		return false, (got/chunkSize + 1) * chunkSize
	}
	return false, got
}

// pathPlaybackActive reports confirmed or inferred playback on path.
func pathPlaybackActive(path string) bool {
	val, ok := playbackRegistry.Load(path)
	if !ok {
		return false
	}
	ps, ok := val.(*PlaybackState)
	return ok && (ps.GetStatus() || ps.IsInferredPlayback())
}

// pieceMapFor describes the pieces around the furthest primary player of hash; nil when none is playing.
func pieceMapFor(hash string) *piecemap.Map {
	if hash == "" {
		return nil
	}
	var h *MkvHandle
	var off int64
	activeHandles.Range(func(key, _ interface{}) bool {
		c := key.(*MkvHandle)
		if c.hash != hash || !c.isPrimaryHandle.Load() {
			return true
		}
		if o := atomic.LoadInt64(&c.lastOff); o > off && !streampolicy.ShouldServeTailFromSSD(o, c.size, warmup.TailWarmupSize) {
			h, off = c, o
		}
		return true
	})
	// Music and audiobook files are a few MB: the map says nothing there, so none is served.
	if h == nil || isAudioSectionPath(h.path) {
		return nil
	}
	tr := eng.Get(hash)
	if tr == nil || tr.Store() == nil {
		return nil
	}
	var file *engine.File
	files := tr.Files()
	for i := range files {
		if files[i].ID == h.fileID && files[i].Length == h.size {
			file = &files[i]
			break
		}
	}
	for i := range files {
		if file == nil && files[i].Length == h.size {
			file = &files[i]
		}
	}
	if file == nil {
		return nil
	}
	src := piecemap.Source{
		Hash: hash, PieceLen: tr.Store().PieceLen(), FileOffset: file.Offset, FileSize: h.size, PlayerOff: off,
		// What the engine keeps ahead of a player: the FUSE read-ahead budget plus the piece cache.
		SpanBytes: int64(gc().ReadAheadBudget),
		Window: func(first, n int) []piecemap.Piece {
			ws := tr.PieceWindow(first, n)
			out := make([]piecemap.Piece, len(ws))
			for i, p := range ws {
				out[i] = piecemap.Piece{Complete: p.Complete, Length: p.Length, Missing: p.Missing, Deadline: p.Deadline}
			}
			return out
		},
		Covered: func(o, e int64) bool { return raCache.Holds(h.path, o, e) },
	}
	// The SSD warmup serves the head (and the tail) itself and the pump skips that zone, so those
	// pieces are never fetched from the torrent: they still count as ready.
	if dw := warmup.DiskWarmup; dw != nil && h.usesWarmup {
		head := dw.GetAvailableRange(hash, h.fileID)
		var tailFrom int64 = -1
		if dw.TailReady(hash, h.fileID) {
			tailFrom = h.size - warmup.TailWarmupSize
		}
		ram := src.Covered
		src.Covered = func(o, e int64) bool {
			return e <= head || (tailFrom >= 0 && o >= tailFrom) || ram(o, e)
		}
	}
	if eng != nil {
		src.SpanBytes += eng.Config().CacheSize
	}
	return piecemap.Build(src)
}
