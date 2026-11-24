package app

import (
	"context"
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/readstats"
	"tiramisu/internal/vfs"
	"tiramisu/internal/warmup"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// shortAwayFromEOF reports whether a read of n bytes into a want-sized buffer at off leaves a
// gap before end of file. Short AT eof is normal; short before it is what the kernel turns
// into a cached zero-filled page.
func shortAwayFromEOF(n, want int, off, size int64) bool {
	return n < want && off+int64(n) < size
}

// Read never hands the kernel a short read away from EOF. FUSE here runs on the kernel page
// cache (no direct_io), and Linux zero-fills the tail of a short read and marks the page
// uptodate — that region would then serve zeros for the life of the mount. Missing bytes are
// refetched; if they still cannot be produced, EIO is returned, which the kernel does not
// cache, so the read is retried instead of a hole being made permanent.
func (h *MkvHandle) Read(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	res, errno := h.readInner(fuseCtx, dest, off)
	if errno != 0 || res == nil {
		return res, errno
	}

	total := res.Size()
	if !shortAwayFromEOF(total, len(dest), off, h.size) {
		return res, 0
	}

	// Short away from EOF: every readInner return site writes into dest, so the bytes
	// already gathered stay valid and only the tail needs filling.
	fuseShortReadCount.Add(1)
	deadline := time.Now().Add(shortReadRefillBudget)

	for attempt := 0; attempt < shortReadRefillAttempts && shortAwayFromEOF(total, len(dest), off, h.size); attempt++ {
		if fuseCtx.Err() != nil {
			return nil, syscall.EINTR
		}
		if time.Now().After(deadline) {
			break // a blocked FUSE read puts smbd in D-state; bail out rather than hang
		}
		sub, subErrno := h.readInner(fuseCtx, dest[total:], off+int64(total))
		if subErrno != 0 || sub == nil {
			break
		}
		m := sub.Size()
		if m == 0 {
			break
		}
		total += m
	}

	if shortAwayFromEOF(total, len(dest), off, h.size) {
		fuseShortReadFailed.Add(1)
		logger.Printf("[ShortRead] Unfillable %d/%d bytes at offset %d for %s - EIO (a partial read would cache zeros)",
			total, len(dest), off, filepath.Base(h.path))
		ttffReadFailed(h.path)
		return nil, syscall.EIO
	}

	fuseShortReadRepaired.Add(1)
	return fuse.ReadResultData(dest[:total]), 0
}

// writeWarmupFromFetch mirrors a fetched block into the SSD warmup files: the head window
// always, the tail only until playback is confirmed (the frozen tail is the discovery snapshot).
// Runs off the read path, from FetchAhead's completion callback.
func writeWarmupFromFetch(h *MkvHandle, off int64, data []byte) {
	if !h.usesWarmup || warmup.DiskWarmup == nil || h.hash == "" || len(data) == 0 {
		return
	}
	if off <= warmup.FileSize {
		warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, off)
		return
	}
	if h.size <= warmup.TailWarmupSize || off < h.size-warmup.TailWarmupSize {
		return
	}
	if val, ok := playbackRegistry.Load(h.path); ok {
		ps := val.(*PlaybackState)
		ps.mu.RLock()
		confirmed := !ps.ConfirmedAt.IsZero()
		ps.mu.RUnlock()
		if confirmed {
			return
		}
	}
	warmup.DiskWarmup.WriteTail(h.hash, h.fileID, data, off, h.size)
}

func (h *MkvHandle) readInner(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	now := time.Now()
	timing := &readstats.ReadTiming{StartTime: now}
	defer func() {
		timing.TotalTime = time.Since(timing.StartTime)
		globalProfilingStats.RecordRead(timing)
	}()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	if errno := h.awaitWake(fuseCtx); errno != 0 {
		return nil, errno
	}

	h.mu.Lock()
	// Late hash recovery: if Open() failed to resolve (metadata lag), retry now.
	if h.hash == "" && h.url != "" {
		if hash, fileID, err := resolveTargetFileFn(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[LateResolution] Recovered hash for %s: %s", filepath.Base(h.path), h.hash[:8])
			go h.pumpOnce.Do(func() {
				h.startNativePump(h.hash, h.fileID)
			})
		}
	}

	idleTime := now.Sub(h.lastActivityTime)
	isFirstBlock := (off == 0) || (idleTime > time.Duration(gc().WarmStartIdleSeconds)*time.Second)
	h.lastActivityTime = now

	// V750: Update PlaybackState inference tracking
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			ps.mu.Lock()
			ps.ReadCount++
			ps.LastReadAt = now
			// Tail-zone reads (MKV Cues probes) and unconfirmed reads (Plex background scans)
			// must never count as real playback progress here.
			if shouldRecordResumeOff(ps.IsHealthy, h.isPrimaryHandle.Load(), off, h.size) {
				ps.LastSeekOff = off
			}
			ps.mu.Unlock()
		}
	}

	if now.Sub(h.lastGlobalUpdate) > 1*time.Minute {
		globalCleanupManager.UpdateActivity(h.path)
		h.lastGlobalUpdate = now
	}
	h.mu.Unlock()

	prevOff := atomic.LoadInt64(&h.lastOff)
	headerRead := h.classifyHeaderRead(prevOff, off, len(dest))
	h.storeReadOff(headerRead, off)

	h.promoteIfStreaming(prevOff, off, len(dest))
	h.boostColdStart(off)

	// Transition WARMUP→STREAMING on resume (first read >= warmup.FileSize), seek (jump > budget),
	// or plain sequential progression past the warmup zone (no seek ever happens during linear
	// playback from offset 0 - without this case, h.state stayed stuck in stateWarmup for the
	// entire rest of playback, paying GetAvailableRange's uncached filepath.Join/Clean cost on
	// every single read instead of the cheap steady-state path; found via CPU profiling a 4K HDR
	// stream where this uncached path work reached ~29% of total CPU).
	// Checked after SSD path above so initial reads within warmup zone are still served.
	if h.state.Load() == stateWarmup {
		isSeek := false
		naturalProgression := false
		if prevOff == -1 {
			if off >= warmup.FileSize {
				isSeek = true
			}
		} else if off != 0 && !headerRead {
			budget := int64(gc().ReadAheadBudget)
			if off > prevOff+budget || prevOff > off+budget {
				isSeek = true
			} else if off >= warmup.FileSize {
				naturalProgression = true
			}
		}
		if isSeek || naturalProgression {
			h.state.Store(stateStreaming)
			if naturalProgression {
				logger.Printf("[Warmup] Sequential read past warmup zone (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
			} else {
				logger.Printf("[Warmup] Seek/Resume detected (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
			}

			// WarmLanding on new handle: pre-fetch chunk at resume position while pump catches
			// up. Fires only for new handles (prevOff==-1) since V286b covers existing handles.
			if prevOff == -1 && h.hash != "" {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// Non-blocking slot check before spawning: under a saturated semaphore (e.g.
					// a Plex scan burst touching hundreds of files), spawning first and waiting
					// inside the goroutine meant a goroutine was created only to sit blocked for
					// up to 300ms doing nothing. A `default` check costs microseconds and avoids
					// the spawn entirely when there's no room, without blocking this Read() call.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.PrefetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								logger.Printf("[WarmLanding] Pre-fetched chunk at %dMB for new handle", goOff/(1024*1024))
							}
						})
					default:
						inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Interrupt pump on genuine seeks; skip reads served from the SSD tail warmup (the
	// pump must stay alive for the rest of the file, and the tail read is already served).
	// V286b: interruptPending prevents cascade — when multiple handles share a pump,
	// only the first to detect the seek fires Interrupt(); others skip until the pump
	// restarts and resets the flag (preventing the thrash loop seen in the logs).
	budget := int64(gc().ReadAheadBudget)
	seekReader := h.nativeReader // capture: a concurrent Release nils the field
	if seekReader != nil && !headerRead && shouldInterruptPump(prevOff, off, budget, shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize),
		prevOff >= 0 && shouldServeTailFromSSD(prevOff, h.size, warmup.TailWarmupSize)) {
		// Promote to primary on a genuine seek once playback is confirmed/inferred - mirrors the
		// refCount 0->1 "primary reconnect" promotion at attach time (startNativePump, pump.go): a secondary
		// (attached) handle doing a real seek during confirmed playback IS the player, and needs
		// isPrimaryHandle so the pump loop's playerOff sync and Release's ps.playerOff
		// persistence (see nativePump) pick it up instead of only the original primary handle,
		// which may be an idle stale probe by now.
		// Only a handle that has streamed like a player qualifies, and never over a primary that is
		// watching: Plex's sparse probes (chapter thumbnails, analysis) would otherwise take the
		// pump with every far read (fusesim metadata_probe_same_file: 8 moves, 9 interrupts).
		if !h.isPrimaryHandle.Load() && h.streamedOnce.Load() && !primaryStreamingFor(h.path, h) {
			if val, ok := playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok && (pbs.GetStatus() || pbs.IsInferredPlayback()) {
					becomePrimary(h)
				}
			}
		}
		var ps *NativePumpState
		if val, ok := activePumps.Load(h.path); ok {
			ps = val.(*NativePumpState)
		}
		// The pump follows the primary: a secondary's seek has nothing of the pump to interrupt.
		if h.isPrimaryHandle.Load() && (ps == nil || !ps.interruptPending.Swap(true)) {
			seekReader.Interrupt()
			eng.ResetShield(h.hash)
			h.state.Store(stateStreaming)
			logger.Printf("[V286b] Interrupt pump for seek+shield reset: %dMB → %dMB (%s→%s)",
				prevOff/(1024*1024), off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))

			// Warm landing zone: pre-fetch the first chunk at the seek target while the
			// pump sleeps its 200ms retry. Starts anacrolix piece download ~200ms earlier,
			// reducing FetchBlock blocking time in Read() and preventing smbd D-state.
			if h.hash != "" {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// See the matching comment on the other WarmLanding site above: non-blocking
					// slot check before spawning avoids creating a goroutine that just blocks.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.PrefetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								logger.Printf("[WarmLanding] Pre-fetched chunk at %dMB for seek target", goOff/(1024*1024))
							}
						})
					default:
						inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Serve warmup zone from SSD (up to 80MB with boundary chunk); stateWarmup gate skips SSD on resume/seek.
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if off < warmupCoverage {
			n, _ := warmup.DiskWarmup.ReadAt(h.hash, h.fileID, dest, off)
			if n > 0 {
				timing.UsedCache = true
				timing.BytesRead = n
				if off == 0 {
					logger.Printf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024)
				}
				h.storeReadOff(headerRead, off)

				h.mu.Lock()
				h.lastLen = n
				h.lastTime = now
				h.mu.Unlock()
				ttffRead(h.path, srcWarmupHead, time.Since(now), n, off)
				return fuse.ReadResultData(dest[:n]), 0
			}
		}
	}

	// Serve tail from SSD whenever the read lands in the tail warmup coverage, regardless
	// of handle state: the WARMUP→STREAMING seek/resume transition above runs before any
	// state-gated tail check could - a resume in the last 16MB was served by network
	// FetchBlock instead of the complete SSD tail file (~16s TTFF on a 36.6GB remux,
	// 2026-08-06). ReadTail is range-safe: returns (0,nil) when the tail file's coverage
	// doesn't include the offset.
	if h.usesWarmup && warmup.DiskWarmup != nil && shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize) {
		n, _ := warmup.DiskWarmup.ReadTail(h.hash, h.fileID, dest, off, h.size)
		if n > 0 {
			timing.UsedCache = true
			timing.BytesRead = n
			h.mu.Lock()
			h.lastLen, h.lastTime = n, now
			h.mu.Unlock()
			ttffRead(h.path, srcWarmupTail, time.Since(now), n, off)
			return fuse.ReadResultData(dest[:n]), 0
		}

		// On SSD tail miss, a blocking demand read beside the head pump (the FUSE read waits on it).
		nFetch, err := nativeBridge.FetchBlockBlocking(h.hash, h.fileID, off, dest)
		if err == nil && nFetch > 0 {
			if h.usesWarmup && warmup.DiskWarmup != nil {
				warmup.DiskWarmup.WriteTail(h.hash, h.fileID, dest[:nFetch], off, h.size)
			}

			timing.UsedCache = false
			timing.BytesRead = nFetch
			h.mu.Lock()
			h.lastLen, h.lastTime = nFetch, now
			h.mu.Unlock()
			ttffRead(h.path, srcFetchBlock, time.Since(now), nFetch, off)
			return fuse.ReadResultData(dest[:nFetch]), 0
		}
	}
	end := off + int64(len(dest)) - 1

	cacheStart := time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		timing.CacheHitTime = time.Since(cacheStart)
		timing.UsedCache = true
		timing.BytesRead = n

		h.storeReadOff(headerRead, off)

		// Predictive prefetch: fetch next chunk if pump is absent or near boundary.
		chunkSize := raCache.ChunkSize(h.path)
		nextChunkStart := (off/chunkSize + 1) * chunkSize

		if (!h.hasSlot.Load() || (nextChunkStart-off < chunkSize/4)) && !raCache.Exists(h.path, nextChunkStart) {
			prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
			if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
				goStart, goKey, goHash, goFileID := nextChunkStart, prefetchKey, h.hash, h.fileID
				fetchEnd := goStart + chunkSize - 1
				if fetchEnd >= h.size {
					fetchEnd = h.size - 1
				}
				// Non-blocking slot check before spawning: avoids creating a goroutine that just
				// blocks up to 500ms waiting on a saturated semaphore during a Plex scan burst
				// (same reasoning as the WarmLanding sites above).
				if fetchEnd <= goStart {
					inFlightPrefetches.Delete(prefetchKey)
				} else {
					select {
					case masterDataSemaphore <- struct{}{}:
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()

							if goHash != "" {
								bufPtr := readBufferPool.Get().(*[]byte)
								defer readBufferPool.Put(bufPtr)

								limit := int64(len(*bufPtr))
								if fetchEnd-goStart+1 < limit {
									limit = fetchEnd - goStart + 1
								}

								n, err := nativeBridge.PrefetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
								if err == nil && n > 0 {
									raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
								}
							}
							// HTTP Fallback REMOVED
						})
					default:
						inFlightPrefetches.Delete(prefetchKey)
					}
				}
			}
		}

		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()

		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	target := int(end - off + 1)
	// h.lastOff already holds off here (stored above), so sequentiality is judged on prevOff.
	h.mu.Lock()
	prevLen := int64(h.lastLen)
	h.mu.Unlock()
	isSeq := prevOff >= 0 && abs(off-(prevOff+prevLen)) <= gc().SequentialTolerance
	// Only unconfirmed probes lose the window. A handle's first read, the primary and any read
	// during playback keep it: before promotion the player is often a secondary reading 64KB
	// blocks, and a target-sized fetch would cost resume and seek one more round trip.
	isStreaming := (len(dest) >= int(gc().StreamingThreshold)) || isSeq || isFirstBlock ||
		prevOff < 0 || h.isPrimaryHandle.Load() || pathPlaybackActive(h.path)
	timing.IsStreaming = isStreaming

	fetchEnd := end
	var fetchSize int64 = int64(target)
	if isStreaming {
		raSize := int64(gc().ReadAheadBase)

		if isFirstBlock {
			raSize = int64(gc().ReadAheadInitial)
		}

		fetchEnd = off + raSize - 1
		if fetchEnd >= h.size {
			fetchEnd = h.size - 1
		}
		fetchSize = fetchEnd - off + 1
	}
	h.mu.Lock()
	h.lastLen, h.lastTime = len(dest), now
	h.mu.Unlock()
	h.storeReadOff(headerRead, off)

	if !h.hasSlot.Load() {
		pumpCreationMu.Lock()

		// pumpCreationMu serializes attaches on this path: a concurrent read on this same
		// handle may have attached while we waited, and a double attach would pin the pump in
		// refCount. The hasSlot write itself is guarded by h.mu inside attachToExistingPump.
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
			goto pumpSlotResolved
		}
		// Attach to existing pump if one is already running for this path.
		if val, ok := activePumps.Load(h.path); ok {
			ps := val.(*NativePumpState)
			if h.attachToExistingPump(ps) {
				logger.Printf("[V258] Handle ATTACHED to existing active pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path))
			}
		}
		// Unlock early if attached or not needed
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
		} else {
			// On-the-fly pump upgrade for confirmed playback with available slot.
			if isStreaming && h.hash != "" {
				if val, ok := playbackRegistry.Load(h.path); ok {
					if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
						select {
						case masterDataSemaphore <- struct{}{}:
							h.hasSlot.Store(true)
							upReader := nativeBridge.NewStreamReader(h.hash, h.fileID, h.size)
							h.nativeReader = upReader
							pumpCtx, pumpCancel := context.WithCancel(context.Background())
							h.pumpCancel = pumpCancel

							sharedState := &NativePumpState{
								cancel:   pumpCancel,
								reader:   upReader,
								path:     h.path,
								refCount: 1,
							}
							activePumps.Store(h.path, sharedState)
							h.pumpState = sharedState
							globalOpenTracker.Inc(h.hash, h.path)

							logger.Printf("[Pump] Upgraded on-the-fly for confirmed playback: %s", filepath.Base(h.path))

							if eng.Get(h.hash) != nil {
								nativeBridge.SetAggressive(h.hash, true, GetEffectiveConcurrencyLimit())
								logger.Printf("[Pump] Aggressive mode enabled on-the-fly for: %s", h.hash[:8])
							}

							upgradedState := sharedState
							safeGo(func() {
								h.nativePump(pumpCtx, upReader, off, upgradedState)
							})
						default:
							// Reserve full, stay in burst mode for now
						}
					}
				}
			}
			pumpCreationMu.Unlock() // Final unlock
		}
	}

pumpSlotResolved:
	// If still no slot (scan or reserve full), acquire a temporary slot for this read
	if !h.hasSlot.Load() {
		select {
		case masterDataSemaphore <- struct{}{}:
			defer func() { <-masterDataSemaphore }()
		case <-fuseCtx.Done():
			return nil, syscall.EINTR
		case <-time.After(30 * time.Second):
			logger.Printf("[MasterSemaphore] Timeout waiting for slot: %s", filepath.Base(h.path))
			return nil, syscall.ETIMEDOUT
		}
	}

	// Rate limiting for non-streaming (metadata) requests only; streaming bypasses to preserve playback priority.
	if !isStreaming {
		rateLimitCtx, rateLimitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rateLimitCancel()
		if err := globalRateLimiter.Acquire(rateLimitCtx); err != nil {
			logger.Printf("Rate limit timeout: %v", err)
			return nil, vfs.ToErrno(err)
		}
	}

	// Queueing (semaphore/rate-limiter) is not cache service time — re-stamp.
	cacheStart = time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		h.storeReadOff(headerRead, off)
		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()
		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	// FALLBACK: Data Fetch with V265 Retry
	// If cache miss, use FetchAhead. Retry up to 3 times if torrent not ready (async Wake).
	var n int

	// releaseFlight is set when this read leads the singleflight below. The read-ahead window
	// keeps filling after this function returns, so the flight is released from the fill's
	// completion callback (once the cache is populated) instead of on return - a follower woken
	// earlier would find nothing cached and start a duplicate fetch of the same chunk.
	releaseFlight := func() {}
	fetchHandedOff := false
	belowLeader := false

	// Dedup concurrent misses in the same chunk: leader fetches, followers wait and copy.
	if h.hash != "" {
		flightChunk := raCache.ChunkSize(h.path)
		flightKey := fmt.Sprintf("%s:%d", h.path, (off/flightChunk)*flightChunk)
		newFlight := &fetchFlight{done: make(chan struct{}), start: off}
		if val, loaded := inFlightFetches.LoadOrStore(flightKey, newFlight); loaded && off < val.(*fetchFlight).start {
			// The leader's window starts above us and keys the same raCache slot: waiting would
			// last the whole window, and a windowed Put of ours would overwrite the leader's.
			// Fetch just our bytes and leave raCache to the leader.
			belowLeader = true
		} else if loaded {
			fl := val.(*fetchFlight)
			copyStart := time.Now()
			// The leader re-caches its window as it fills, so poll instead of waiting for the
			// whole window: a follower one FUSE block behind the leader gets its bytes after one
			// fill step, not after the remaining 15MB.
			// While blocked here the leader's window stays a demand (FetchAhead demotes it once
			// its head is delivered); awaitDone releases that on every way out of the wait.
			awaitDone := nativeBridge.AwaitFill(h.hash, h.fileID, off)
			leaderDone := false
			for {
				if nCopy := raCache.CopyTo(h.path, off, end, dest); nCopy > 0 {
					awaitDone()
					fetchFlightDedupCount.Add(1)
					timing.UsedCache = true
					timing.BytesRead = nCopy
					h.storeReadOff(headerRead, off)
					h.mu.Lock()
					h.lastLen = nCopy
					h.lastTime = now
					h.mu.Unlock()
					ttffRead(h.path, srcRACacheHit, time.Since(copyStart), nCopy, off)
					return fuse.ReadResultData(dest[:nCopy]), 0
				}
				if leaderDone {
					break // leader finished without covering our range
				}
				select {
				case <-fl.done:
					leaderDone = true // one more CopyTo, then fall through
				case <-fuseCtx.Done():
					awaitDone()
					return nil, syscall.EINTR
				case <-time.After(20 * time.Millisecond):
				}
			}
			awaitDone()
			// Leader failed or missed our range — fetch directly, don't re-register.
		} else {
			var once sync.Once
			releaseFlight = func() {
				once.Do(func() {
					inFlightFetches.Delete(flightKey)
					close(newFlight.done)
				})
			}
			defer func() {
				if !fetchHandedOff {
					releaseFlight()
				}
			}()
		}
	}

	if h.hash != "" {
		// V753: the very first read of a session can miss raCache/warmup even when
		// headReady was already true at Open() (partial coverage from a prior open),
		// which skips forceTorrentWarmupActive there (VirtualMkvNode.Open in fuse_file.go, gated on
		// !headReady). That leaves hedgeWatchdog not running yet - playback-pressure
		// hasn't engaged either, since the pump has barely started - so this exact
		// fetch has zero hedge protection. Force it here instead, bounded to the
		// warmup head window: forcing it for a post-idle resume deep in the file
		// (isFirstBlock is also true after WarmStartIdleSeconds) would set
		// warmupActive with no way back to false, since processWrite's off>FileSize
		// early return (warmup.go) means COMPLETED - the only call site that clears
		// it - never fires for writes past the head window.
		if h.usesWarmup && isFirstBlock && off <= warmup.FileSize {
			forceTorrentWarmupActive(h.hash, h.fileID)
		}
		fetchStart := time.Now()

		for attempt := 0; attempt < 3; attempt++ {
			bufPtr := readBufferPool.Get().(*[]byte)

			limit := fetchSize
			if limit > int64(len(*bufPtr)) {
				limit = int64(len(*bufPtr))
			}
			buf := (*bufPtr)[:limit]

			// The window keeps filling after this read returns. Re-cache on every step so the
			// reads queued behind this one find their bytes as they land instead of waiting for
			// the whole window; the buffer goes back to the pool only once the fill is done.
			if belowLeader && limit > int64(target) {
				buf = buf[:target]
			}
			nFetch, err := nativeBridge.FetchAhead(h.hash, h.fileID, off, buf, dest[:target],
				func(total int, done bool, _ error) {
					if total > 0 && !belowLeader {
						raCache.Put(h.path, off, off+int64(total)-1, buf[:total])
					}
					if !done {
						return
					}
					if total > 0 {
						writeWarmupFromFetch(h, off, buf[:total])
					}
					releaseFlight()
					readBufferPool.Put(bufPtr)
				})
			if err == nil && nFetch > 0 {
				n = nFetch
				fetchHandedOff = true
				timing.HTTPFetchTime = time.Since(fetchStart)
				goto DATA_READY
			}
			if attempt < 2 {
				select {
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}

	// Terminal once the bounded retries are spent. EAGAIN reads as "try again" to
	// a scanner, which then blocks or loops on bytes that are never coming.
	ttffReadFailed(h.path)
	return nil, syscall.EIO

DATA_READY:

	timing.BytesRead = target

	if n > 0 {
		// The bytes are already in dest and the read-ahead window behind them is still
		// filling; raCache.Put and the SSD warmup writes happen in FetchAhead's callback.
		ttffRead(h.path, srcFetchBlock, timing.HTTPFetchTime, n, off)

		// Update sequential detection state
		h.storeReadOff(headerRead, off)
		h.mu.Lock()
		h.lastLen = target
		h.mu.Unlock()

		globalCleanupManager.UpdateOffset(h.path, off, target)

		// Prefetch next chunk if in last 25% of current chunk and pump is absent or lagging.
		chunkSize := raCache.ChunkSize(h.path)
		currentChunkIndex := off / chunkSize
		nextChunkStart := (currentChunkIndex + 1) * chunkSize
		distanceToNext := nextChunkStart - off

		maxCached := raCache.MaxCachedOffset(h.path)
		isLagging := maxCached < nextChunkStart

		if isStreaming && (!h.hasSlot.Load() || isLagging) {
			if distanceToNext < chunkSize/4 {
				prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
					goStart, goSize, goKey, goHash, goFileID := nextChunkStart, chunkSize, prefetchKey, h.hash, h.fileID
					safeGo(func() {
						defer inFlightPrefetches.Delete(goKey)

						// Check if already in cache to avoid useless work
						if raCache.Exists(h.path, goStart) {
							return // Already cached
						}

						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := globalRateLimiter.Acquire(ctx); err != nil {
							return
						}

						fetchEnd := goStart + goSize - 1
						if fetchEnd >= h.size {
							fetchEnd = h.size - 1
						}
						if fetchEnd <= goStart {
							return
						}

						select {
						case masterDataSemaphore <- struct{}{}:
							defer func() { <-masterDataSemaphore }()
						default:
							return // Skip if pool is saturated
						}

						// Zero-Network Native Prefetch (V227 Phase 7)
						if goHash != "" {
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)

							limit := int64(len(*bufPtr))
							if fetchEnd-goStart+1 < limit {
								limit = fetchEnd - goStart + 1
							}

							n, err := nativeBridge.PrefetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
							}
							return
						}

						// HTTP Fallback REMOVED
					})
				}
			}
		}

		return fuse.ReadResultData(dest[:n]), 0
	}

	return fuse.ReadResultData(dest[:0]), 0
}
