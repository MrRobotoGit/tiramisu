package app

import (
	"context"
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/warmup"
)

// ownsPumpState reports whether a releasing handle still owns the pump registered
// for its path. activePumps is keyed by path and the state is replaced wholesale,
// so a handle that outlived its pump would otherwise decrement a count it never
// incremented, driving refCount negative and firing the "last handle closed"
// teardown while a live handle is still reading.
func ownsPumpState(handleState, registered *NativePumpState) bool {
	return handleState != nil && handleState == registered
}

func (h *MkvHandle) Release(fuseCtx context.Context) syscall.Errno {
	// The orphan-handle GC releases too; the kernel's later RELEASE must not decrement twice.
	if h.released.Swap(true) {
		return 0
	}
	if h.coldBoostTried.Load() {
		eng.FriendsPlayback(h.hash, fmt.Sprintf("%p", h), "", false)
	}
	ttffReleaseClose(h.path)
	logger.Printf("=== RELEASE VIRTUAL === path=%s", h.path)

	// Delete from activeHandles here, not at the end: the early returns below (probe
	// without slot, replaced pump) would otherwise leak this entry, and both the live
	// handle checks (anyLiveHandleFor*) and cleanup's activePaths trust the map.
	activeHandles.Delete(h)

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		// Only primary handles persist position; secondary probes have arbitrary offsets.
		if h.isPrimaryHandle.Load() {
			if pos := atomic.LoadInt64(&h.lastOff); pos > 0 {
				atomic.StoreInt64(&ps.playerOff, pos)
			}
		}
		// Only decrement if this handle acquired a slot; probe/header reads must not decrement.
		if !h.hasSlot.Load() {
			return 0
		}
		globalOpenTracker.Dec(h.hash, h.path)
		if !ownsPumpState(h.pumpState, ps) {
			logger.Printf("[Pump] Stale handle for %s: pump was replaced, refCount left alone", filepath.Base(h.path))
			return 0
		}
		newRefs := atomic.AddInt32(&ps.refCount, -1)
		logger.Printf("[Pump] Release handle for %s (Remaining Refs: %d)", filepath.Base(h.path), newRefs)

		if newRefs <= 0 {
			// V306: If playback is healthy (webhook media.play confirmed), skip grace period.
			// The pump stays alive until media.stop webhook sets IsHealthy=false and kills it.
			// This survives long pauses, buffering gaps, and Apple TV re-reads without
			// killing the pump and causing freeze on resume.
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); pbState.GetStatus() && pbState.takeIgnoredStop(time.Now()) {
					// The stop was guarded only because this handle was still open.
					path, closed := h.path, time.Now()
					logger.Printf("[PLEX] Deferred STOP: last handle closed for %s, applying in %s unless it reopens", filepath.Base(path), deferredStopGrace)
					time.AfterFunc(deferredStopGrace, func() {
						if anyLiveHandleFor(path) || pbState.confirmedAfter(closed) || !pbState.GetStatus() {
							logger.Printf("[PLEX] Deferred STOP dropped: %s reopened or confirmed again", filepath.Base(path))
							return
						}
						applyPlexStop(path, pbState, "media.stop (deferred)")
					})
					goto skipGrace
				} else if pbState.IsHealthy {
					logger.Printf("[V306] Healthy playback — pump stays alive (no grace period) for %s", filepath.Base(h.path))
					// Stop any pending grace timer from a previous release cycle
					if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
						oldTimer.(*time.Timer).Stop()
					}
					goto skipGrace
				}
			}

			// Grace period: 30s for unconfirmed probes/scans.
			graceDuration := 30 * time.Second
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); !pbState.ConfirmedAt.IsZero() {
					graceDuration = 90 * time.Second
					if pbState.IsInferredPlayback() {
						graceDuration = 5 * time.Minute // V750
						logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
					}
				} else if pbState.IsInferredPlayback() {
					graceDuration = 5 * time.Minute // V750
					logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
				}
			}
			if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
				oldTimer.(*time.Timer).Stop()
			}
			var t *time.Timer
			t = time.AfterFunc(graceDuration, func() {
				pumpTimers.CompareAndDelete(h.path, t)
				if val, ok := activePumps.Load(h.path); ok {
					psNow := val.(*NativePumpState)
					if atomic.LoadInt32(&psNow.refCount) <= 0 {
						if psNow.cancel != nil {
							psNow.cancel()
						}
						activePumps.Delete(h.path)
						logger.Printf("[V420] Shared Pump Terminated (Grace Period Expired): %s", filepath.Base(h.path))
					}
				}
			})
			pumpTimers.Store(h.path, t)
			logger.Printf("[V420] Last handle closed: Shared Pump entering %s grace period for %s", graceDuration, filepath.Base(h.path))
			// No media.stop may follow (scan, players without webhooks): fill the tail once idle.
			if v, ok := tailFillTargets.Load(h.path); ok && warmup.DiskWarmup != nil {
				tgt, path := v.(tailFillTarget), h.path
				warmup.DiskWarmup.EnsureTailWhenIdle(tgt.hash, tgt.fileID, tgt.size, func() bool {
					return !anyLiveHandleFor(path) && !hashInPlayback(tgt.hash)
				})
			}
		}

	skipGrace:
	}

	// Nil local reference only; pump goroutine owns the reader lifecycle via captured copy.
	h.nativeReader = nil

	// Fast-drop (5s) for scanner probes never confirmed by webhook; 30s otherwise.
	retentionDelay := 30 * time.Second
	lastOffset := atomic.LoadInt64(&h.lastOff)
	isProbeOnly := lastOffset < 2*1024*1024 // < 2MB = probe/scanner, not real playback
	if h.hasWarmup && isProbeOnly {
		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			state.mu.RLock()
			stopped := state.IsStopped
			everConfirmed := !state.ConfirmedAt.IsZero()
			state.mu.RUnlock()
			// Fast-drop only if: explicitly stopped OR never confirmed by any webhook
			if stopped || !everConfirmed {
				retentionDelay = 5 * time.Second
			}
		}
	}

	if oldTimer, ok := priorityTimers.LoadAndDelete(h.path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(retentionDelay, func() {
		priorityTimers.CompareAndDelete(h.path, t)

		// V750: Don't remove priority if pump is still active for this path
		if _, pumpOk := activePumps.Load(h.path); pumpOk {
			logger.Printf("[V750] Priority retained — pump still active for %s", filepath.Base(h.path))
			return
		}

		// V306: Don't remove priority if playback is confirmed healthy — even if
		// pump goroutine exited unexpectedly, the torrent must stay alive for the
		// next Read() to create a new pump without a gap.
		if val, ok := playbackRegistry.Load(h.path); ok {
			if pbState := val.(*PlaybackState); pbState.GetStatus() {
				logger.Printf("[V306] Priority retained — healthy playback confirmed for %s", filepath.Base(h.path))
				return
			}
		}

		// O(1): controlla se il path ha ancora handle aperti prima di disabilitare priority.
		if globalOpenTracker.IsPathOpen(h.path) {
			return
		}

		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			if state.Hash != "" {
				// O(1): controlla se altri file dello stesso torrent sono ancora aperti.
				if globalOpenTracker.IsHashOpen(state.Hash) {
					// Another file from the same torrent is still open, keep priority.
					return
				}

				if t := eng.Get(state.Hash); t != nil {
					t.IsPriority.Store(false)
					nativeBridge.SetAggressive(state.Hash, false, 0) // aggrOn is sticky in the streamer

					// Fast-drop scanner handles never confirmed by webhook.
					scannerDrop := false
					if h.hasWarmup && isProbeOnly {
						state.mu.RLock()
						everConfirmed := !state.ConfirmedAt.IsZero()
						state.mu.RUnlock()
						if !state.IsHealthy && !everConfirmed {
							scannerDrop = true
						}
					}

					if scannerDrop {
						t.AddExpiredTime(5 * time.Second)
						logger.Printf("[V272] Scanner fast-drop for Hash %s (5s retention)", state.Hash)
					} else {
						t.AddExpiredTime(30 * time.Second)
						logger.Printf("[NativeBridge] Priority disabled for Hash %s (All handles closed)", state.Hash)
					}
				}
			}
		}
		// Registry entry kept (without priority) for fast webhook-triggered resume.
		// Cleanup handled by GlobalCleanupManager (15 min timeout).
		// playbackRegistry.Delete(h.path)
	})
	priorityTimers.Store(h.path, t)

	return 0
}
