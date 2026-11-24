package app

import (
	"context"
	_ "net/http/pprof"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/bridge"
)

// handleState values for MkvHandle.state (atomic.Uint32).
// A handle transitions one-way: stateWarmup → stateStreaming.
const (
	stateWarmup    uint32 = 0 // Initial: SSD warmup eligible (TTFF phase)
	stateStreaming uint32 = 1 // Pump-only streaming; no SSD warmup
)

// watchSample is a V752 starvation-watchdog observation: pump offset and player
// offset at time t, used to compute both rates over a sliding 10s window.
type watchSample struct {
	t         time.Time
	offset    int64
	playerOff int64
}

func stateName(s uint32) string {
	switch s {
	case stateWarmup:
		return "WARMUP"
	case stateStreaming:
		return "STREAMING"
	default:
		return "UNKNOWN"
	}
}

type MkvHandle struct {
	url, path        string
	size             int64
	lastOff          int64
	lastLen          int
	lastTime         time.Time
	lastActivityTime time.Time
	monitorStarted   bool
	lastGlobalUpdate time.Time

	nativeReader *bridge.NativeReader
	hash         string
	magnet       string
	fileID       int
	mu           sync.Mutex
	pumpCancel   context.CancelFunc
	hasSlot      atomic.Bool
	// pumpState is the state this handle incremented. activePumps is keyed by path
	// and the state is replaced wholesale, so Release must not decrement a pump it
	// never counted.
	pumpState       *NativePumpState
	isWatching      bool
	hasWarmup       bool          // true if both head+tail warmup available at Open time
	usesWarmup      bool          // false for audio projections: they skip SSD warmup entirely
	state           atomic.Uint32 // handleState: stateWarmup | stateStreaming
	coldBoostTried  atomic.Bool
	pumpOnce        sync.Once
	isPrimaryHandle atomic.Bool  // pump creator, primary reconnects (refCount 0→1), proven readers
	seqAdvances     atomic.Int32 // consecutive sequential streaming reads, feeds the promotion below
	headerChainEnd  atomic.Int64 // end of the last header read skipped by isHeaderRead, 0 if none
	released        atomic.Bool  // Release ran once (orphan GC or kernel)
	seqRun          atomic.Int32 // consecutive streaming advances, primary or not (feeds streamedOnce)
	streamedOnce    atomic.Bool  // read sequentially like a player at least once
	openedAt        time.Time    // set once at Open
	wakeWait        *pendingWake // activation still running when Open returned; set once
	bitrateKnown    atomic.Bool  // the film bitrate reached the pump reader
	bitrateTried    atomic.Int64 // unix nanos of the last header parse attempt
	mp4HeadTried    atomic.Bool  // an MP4's larger head was searched for its moov once
	mp4TailTried    atomic.Bool  // an MP4's SSD tail was searched for its moov once
}

// pendingWake is a torrent activation Open stopped waiting for.
type pendingWake struct {
	done chan struct{}
	err  error // valid once done is closed
}

// videoWakeGrace is how long Open waits for a cold video activation before handing it to the reads.
const videoWakeGrace = 2 * time.Second

// awaitWake blocks a read until the activation Open left running ends.
func (h *MkvHandle) awaitWake(ctx context.Context) syscall.Errno {
	pw := h.wakeWait
	if pw == nil {
		return 0
	}
	select {
	case <-pw.done:
		if pw.err != nil {
			return syscall.EIO
		}
		return 0
	case <-ctx.Done():
		return syscall.EINTR
	}
}

// anyConfirmedPlayback reports whether any playback is confirmed (IsHealthy).
func anyConfirmedPlayback() bool {
	found := false
	playbackRegistry.Range(func(_, v interface{}) bool {
		if ps, ok := v.(*PlaybackState); ok && ps.GetStatus() {
			found = true
			return false
		}
		return true
	})
	return found
}
