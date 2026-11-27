package app

import (
	"context"
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"tiramisu/internal/monitor/clients"
	"tiramisu/internal/vfs"
	"tiramisu/internal/warmup"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// VirtualMkvNode - nodo per singolo file .mkv virtuale
type VirtualMkvNode struct {
	fs.Inode
	vMeta *vfs.Metadata
	wake  func(context.Context, string, int) error // per-node activation dependency; wired at Open after RED
}

// Compile-time interface checks
var _ fs.NodeGetattrer = (*VirtualMkvNode)(nil)
var _ fs.NodeOpener = (*VirtualMkvNode)(nil)

func (n *VirtualMkvNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttrFromMetadata(n.vMeta, &out.Attr)
	out.Ino = getFileInodeFromMap(n.vMeta.Path)
	return 0
}

var _ fs.NodeSetattrer = (*VirtualMkvNode)(nil)

// Setattr refuses every metadata mutation of an audio projection: chmod, chown and
// truncate are all API-owned (spec 4). Video keeps answering ENOSYS as before.
func (n *VirtualMkvNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if n.vMeta.Audio {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}

func (n *VirtualMkvNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if gc().LogLevel == "DEBUG" {
		logger.Printf("=== OPEN VIRTUAL === path=%s", n.vMeta.Path)
	}
	// Read-only first: an audio projection refuses a writable open before any handle
	// exists, so no write path ever has to exist behind it (spec 4).
	if n.vMeta.Audio && audioWriteIntent(flags) {
		return nil, 0, syscall.EROFS
	}
	if len(n.vMeta.SegmentHeader) > 0 {
		return n.openCueTrack(ctx, flags)
	}

	// PROACTIVE CLEANUP TRIGGER (V246): must be sync before any Read() can arrive.
	raCache.SwitchContext(n.vMeta.Path)

	// Cancel any pending release timers (Debounce)
	if oldTimer, ok := pumpTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	if oldTimer, ok := priorityTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}

	hashStr, urlFileIdx := vfs.ExtractHashAndIndex(n.vMeta.URL)
	clients.NoteFUSEOpen(ctx, hashStr, n.vMeta.Path)

	// hasFullWarmup: Open returns instantly only if both head and tail warmup files are ready.
	// headReady: Allows async Wake and direct ID injection for instant start.
	// Audio projections skip the movie/TV head/tail SSD warmup entirely, and Phase 1
	// puts nothing in its place: demand fetch plus the existing read-ahead is the
	// whole policy. Decided once here rather than special-cased at each warmup site.
	usesWarmup := pathUsesSSDWarmup(n.vMeta.Path)
	// isAudio: audio projections resolve their identity from the registry, which needs no
	// torrent, so the pump can start while the wake runs behind it instead of serializing
	// activation before the first read (see the wake branch).
	isAudio := isAudioSectionPath(n.vMeta.Path)

	headReady := false
	tailReady := false
	if usesWarmup && warmup.DiskWarmup != nil && hashStr != "" {
		headReady = warmup.DiskWarmup.HeadReady(hashStr, urlFileIdx)
		tailReady = warmup.DiskWarmup.TailReady(hashStr, urlFileIdx)
	}
	magnetCandidate := n.vMeta.URL
	if hashStr != "" && (strings.HasPrefix(n.vMeta.URL, "http://") || strings.HasPrefix(n.vMeta.URL, "https://")) {
		magnetCandidate = "magnet:?xt=urn:btih:" + hashStr
	}

	// Async Wake when head warmup is ready (Open returns instantly), and for audio, whose
	// identity comes from the registry and whose pump can start while the wake runs behind it;
	// sync Wake otherwise.
	wake := n.wake
	if wake == nil && nativeBridge != nil {
		wake = nativeBridge.Wake
	}
	var wakeWait *pendingWake
	if wake != nil && magnetCandidate != "" {
		if headReady {
			// Open returns instantly here, so the activation must not inherit the
			// request context: the kernel tears it down as soon as Open returns, and
			// a background context lets the wake finish for the reader that is about
			// to open the same path.
			safeGo(func() {
				if err := wake(context.Background(), magnetCandidate, urlFileIdx); err != nil {
					logger.Printf("[VFS] background activation failed for %s: %v", n.vMeta.Path, err)
				}
			})
		} else if isAudio {
			// Audio has no SSD head to serve the first reads, so an activation that fails at
			// once (semaphore exhausted mid-scan) must fail Open as the synchronous path does;
			// a slower one keeps running behind the pump.
			if err := vfs.StartWake(func(c context.Context) error {
				return wake(c, magnetCandidate, urlFileIdx)
			}, audioWakeGrace, logger.Printf); err != nil {
				logger.Printf("[VFS] activation failed for %s: %v", n.vMeta.Path, err)
				return nil, 0, syscall.EIO
			}
		} else {
			if ctx.Err() != nil {
				return nil, 0, syscall.EINTR
			}
			// Samba runs CREATE on the client's single smbd process, so waiting here for a cold
			// activation (up to the 45s metadata timeout) froze every Plex session of that client.
			// An activation that fails within the grace still fails Open; a slower one goes on in
			// the background and the handle's first read waits for it, on Samba's aio threads.
			pw := &pendingWake{done: make(chan struct{})}
			if err := vfs.StartWake(func(c context.Context) (err error) {
				// done must close even on panic, or the handle's reads would wait forever.
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("activation panicked: %v", r)
					}
					pw.err = err
					close(pw.done)
				}()
				return wake(c, magnetCandidate, urlFileIdx)
			}, videoWakeGrace, logger.Printf); err != nil {
				logger.Printf("[VFS] activation failed for %s: %v", n.vMeta.Path, err)
				return nil, 0, syscall.EIO
			}
			select {
			case <-pw.done:
			default:
				wakeWait = pw
			}
		}
	}

	// Registered only once activation succeeded: a session for an Open that is
	// about to fail outlives the call and is never closed.
	ttffRegister(n.vMeta.Path, n.vMeta.Size, hashStr, headReady, tailReady)

	if val, exists := playbackRegistry.Load(n.vMeta.Path); !exists {
		playbackRegistry.Store(n.vMeta.Path, &PlaybackState{
			Path:                n.vMeta.Path,
			Hash:                hashStr,
			ImdbID:              n.vMeta.ImdbID,
			ExternalID:          n.vMeta.ExternalID,
			ExternalIDNamespace: n.vMeta.ExternalIDNamespace,
			OpenedAt:            time.Now(),
		})
	} else {
		if val != nil {
			state := val.(*PlaybackState)
			state.mu.Lock()
			state.OpenedAt = time.Now()
			state.IsStopped = false

			// Restore priority only if webhook confirmed recently (<30m) to avoid zombie torrents.
			recentlyConfirmed := !state.ConfirmedAt.IsZero() && time.Since(state.ConfirmedAt) < 30*time.Minute
			isHealthy := state.IsHealthy
			state.mu.Unlock()

			if isHealthy && recentlyConfirmed && state.Hash != "" {
				if t := eng.Get(state.Hash); t != nil {
					if !t.IsPriority.Load() {
						t.IsPriority.Store(true)
						logger.Printf("[NativeBridge] Priority RESTORED for Silent Re-Open: %s", state.Hash)
					}
				}
			}
		}
	}

	now := time.Now()

	// Use warmup IDs directly when available, skipping the resolveTargetFile retry loop.
	var finalHash string
	var fileIdx int
	var isNative bool

	if headReady && hashStr != "" {
		finalHash = hashStr
		fileIdx = urlFileIdx
		isNative = true
	} else {
		var err error
		finalHash, fileIdx, err = resolveTargetFileFn(n.vMeta.URL, n.vMeta.Size, n.vMeta.Path)
		isNative = (err == nil)
		if !isNative && gc().LogLevel == "DEBUG" {
			logger.Printf("[NativeBridge] Resolution failed for %s: %v. Access will rely on cache/retry.", filepath.Base(n.vMeta.Path), err)
		}
	}

	h := &MkvHandle{
		wakeWait:         wakeWait,
		openedAt:         time.Now(),
		url:              n.vMeta.URL,
		magnet:           magnetCandidate, // Store for potential re-wake
		size:             n.vMeta.Size,
		path:             n.vMeta.Path,
		lastTime:         now,
		lastOff:          -1,
		lastActivityTime: now,       // Initialize activity tracking
		hasWarmup:        headReady, // Eligibility for fast SSD probes
		usesWarmup:       usesWarmup,
	}
	h.state.Store(stateWarmup) // Initial state; transitions to stateStreaming on seek/resume.

	if isNative {
		h.hash = finalHash
		h.fileID = fileIdx
		// media.stop only knows the path; keep what EnsureTail needs to reach the file.
		if usesWarmup {
			tailFillTargets.Store(n.vMeta.Path, tailFillTarget{hash: finalHash, fileID: fileIdx, size: n.vMeta.Size})
		}
		// Gillian: proactive pump start at Open() — pump ready before first Read().
		// pumpOnce ensures single start; late rescue path in Read() handles hash=='' case.
		h.pumpOnce.Do(func() {
			h.startNativePump(finalHash, fileIdx)
		})
		if usesWarmup && !headReady {
			// Real cold start: no warmup data present yet. Signal warmupActive here, at Open(),
			// rather than waiting for the first WriteChunk - that first-connection burst is
			// exactly the window aggressive PEX churn (Task 3) needs to catch, and by the time
			// WriteChunk fires once, the torrent's initial peer set has often already connected.
			// warmup.OnWarmupStateChange (wired at startup) still owns turning this back off once
			// the writeWorker synchronously observes real STARTING/COMPLETED transitions.
			forceTorrentWarmupActive(finalHash, fileIdx)
		}
		if !headReady && !isAudio { // no local warmup, UseDisk off included: in parallel with the swarm
			eng.FriendsColdOpen(finalHash)
		}
	}

	activeHandles.Store(h, true)
	if !isAudio {
		stallNotices.Open(h, h.path, h.hash, h.fileID, h.size)
	}

	return h, 0, 0
}
