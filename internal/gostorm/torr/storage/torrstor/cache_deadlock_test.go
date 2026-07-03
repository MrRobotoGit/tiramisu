package torrstor

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	torrenttypes "github.com/anacrolix/torrent/types"

	"tiramisu/internal/gostorm/settings"
)

func TestMain(m *testing.M) {
	// Initialize settings.BTsets to avoid nil dereference in MarkNotComplete/GetAdaptiveShield.
	if settings.BTsets == nil {
		settings.BTsets = &settings.BTSets{
			ResponsiveMode: false,
			AdaptiveShield: false,
		}
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Test 5: TestCacheCleanPiecesNoDeadlock
// 200 pieces, 10 goroutines hammer cleanPieces(); must complete in < 5s.
// ---------------------------------------------------------------------------

func TestCacheCleanPiecesNoDeadlock(t *testing.T) {
	const pieceCount = 200
	const pieceLen = int64(512 * 1024) // 512 KB

	c := &Cache{
		capacity:      int64(pieceCount) * pieceLen * 2, // large capacity → no eviction
		pieceLength:   pieceLen,
		pieceCount:    pieceCount,
		pieces:        make(map[int]*Piece),
		readers:       make(map[*Reader]struct{}),
		cleanTrigger:  make(chan struct{}, 1),
		cleanStop:     make(chan struct{}),
		localPriority: make(map[int]torrenttypes.PiecePriority),
		// torrent is intentionally nil → setLoadPriority / clearPriority are no-ops
	}

	for i := 0; i < pieceCount; i++ {
		p := &Piece{Id: i, cache: c, mPiece: &MemPiece{}}
		p.mPiece.piece = p
		c.pieces[i] = p
	}

	// Start the background cleaner goroutine (mirrors NewCache)
	go func() {
		for {
			select {
			case <-c.cleanStop:
				return
			case <-c.cleanTrigger:
				c.cleanPieces()
			}
		}
	}()
	defer close(c.cleanStop)

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					c.cleanPieces()
				}
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success — no deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("TestCacheCleanPiecesNoDeadlock: deadlock detected (timeout after 5s)")
	}
}

// ---------------------------------------------------------------------------
// Test 6: TestAdaptiveShieldSingleWatchdog
// 50 concurrent MarkNotComplete() calls on a piece with a non-nil buffer must
// not spawn more than 1 extra goroutine (the single watchdog).
// ---------------------------------------------------------------------------

func TestAdaptiveShieldSingleWatchdog(t *testing.T) {
	// Reset global shield state so this test is independent of test order.
	ResetShield()
	defer ResetShield()

	const pieceLen = int64(256 * 1024)
	c := &Cache{
		capacity:      pieceLen * 4,
		pieceLength:   pieceLen,
		pieceCount:    1,
		pieces:        make(map[int]*Piece),
		readers:       make(map[*Reader]struct{}),
		cleanTrigger:  make(chan struct{}, 1),
		cleanStop:     make(chan struct{}),
		localPriority: make(map[int]torrenttypes.PiecePriority),
	}
	defer close(c.cleanStop)

	p := &Piece{Id: 0, cache: c, mPiece: &MemPiece{}}
	p.mPiece.piece = p
	// Pre-allocate buffer so the V-evict-guard (buffer != nil) passes
	p.mPiece.buffer = make([]byte, pieceLen)
	c.pieces[0] = p

	baseGoroutines := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.MarkNotComplete()
		}()
	}
	wg.Wait()

	// Give the watchdog goroutine a brief moment to appear in the scheduler.
	time.Sleep(50 * time.Millisecond)

	afterGoroutines := runtime.NumGoroutine()
	delta := afterGoroutines - baseGoroutines

	// Allow at most 1 extra goroutine (the single watchdog).
	if delta > 1 {
		t.Errorf("goroutine leak: started with %d, now %d (delta=%d, want ≤1)",
			baseGoroutines, afterGoroutines, delta)
	}
}

// ---------------------------------------------------------------------------
// Test 7: TestMemPieceConcurrentReadWrite
// 5 writers + 5 readers on the same MemPiece must not panic or data-race.
// Run with -race to catch data races.
// ---------------------------------------------------------------------------

func TestMemPieceConcurrentReadWrite(t *testing.T) {
	const pieceLen = int64(64 * 1024) // 64 KB

	// Minimal cache wiring needed by WriteAt (cleanTrigger + pieceLength).
	c := &Cache{
		capacity:      pieceLen * 4,
		pieceLength:   pieceLen,
		pieceCount:    1,
		pieces:        make(map[int]*Piece),
		readers:       make(map[*Reader]struct{}),
		cleanTrigger:  make(chan struct{}, 1),
		cleanStop:     make(chan struct{}),
		localPriority: make(map[int]torrenttypes.PiecePriority),
	}
	defer close(c.cleanStop)

	p := &Piece{Id: 0, cache: c, mPiece: &MemPiece{}}
	p.mPiece.piece = p
	c.pieces[0] = p

	mp := p.mPiece

	const goroutines = 5
	const iters = 200

	var wg sync.WaitGroup
	var panicCount int32

	safeDo := func(fn func()) {
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt32(&panicCount, 1)
			}
			wg.Done()
		}()
		fn()
	}

	writePayload := make([]byte, 1024)
	for i := range writePayload {
		writePayload[i] = byte(i % 256)
	}

	// Writers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go safeDo(func() {
			for j := 0; j < iters; j++ {
				_, _ = mp.WriteAt(writePayload, 0)
			}
		})
	}

	// Readers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go safeDo(func() {
			readBuf := make([]byte, 1024)
			for j := 0; j < iters; j++ {
				_, _ = mp.ReadAt(readBuf, 0)
			}
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TestMemPieceConcurrentReadWrite: timed out after 5s")
	}

	if n := atomic.LoadInt32(&panicCount); n > 0 {
		t.Errorf("caught %d panic(s) during concurrent read/write", n)
	}
}
