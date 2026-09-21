package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"tiramisu/internal/gostorm/native"
	"tiramisu/internal/ratelimit"
	"tiramisu/internal/vfs"
)

// startupReadFailSafe only bounds how long a test waits for a call that a correct
// implementation completes immediately. It is never used as synchronization: every
// passing path is released by a channel, not by this deadline.
const startupReadFailSafe = 2 * time.Second

// startupReadSections lists one representative path per media section. Audio has no
// SSD warmup and no head/tail zones; movie/TV has both, so the two exercise different
// branches of the same Open/Read code.
var startupReadSections = []struct {
	name string
	path string
}{
	{name: "movie", path: "movies/Cold Start_01234567.mkv"},
	{name: "audio", path: "music/Artist/Cold Start_01234567.flac"},
}

// startupReadClearPath removes state that a deliberately rejected Open must not
// leave behind. It only runs from cleanups, so a regression that does leak state
// cannot bleed into the next test; the assertions themselves never rely on it.
func startupReadClearPath(path string) {
	activeHandles.Range(func(k, _ any) bool {
		if h, ok := k.(*MkvHandle); ok && h.path == path {
			activeHandles.Delete(k)
		}
		return true
	})
	if v, ok := activePumps.Load(path); ok {
		if pump, ok := v.(*NativePumpState); ok && pump.cancel != nil {
			pump.cancel()
		}
		activePumps.Delete(path)
	}
	playbackRegistry.Delete(path)
	sessions.Delete(path)
	tailFillTargets.Delete(path)
}

// startupReadConfigureReaderGlobals supplies only the process globals that main
// normally initialises after configuration loading. The VFS unit harness does
// not run main, while these direct tests intentionally exercise its real
// bounded-fetch path.
func startupReadConfigureReaderGlobals(t *testing.T) {
	t.Helper()
	previousSemaphore, previousLimiter, previousPool := masterDataSemaphore, globalRateLimiter, readBufferPool
	limiter := ratelimit.NewRateLimiter(4, time.Hour)
	masterDataSemaphore = make(chan struct{}, 1)
	globalRateLimiter = limiter
	readBufferPool = &sync.Pool{New: func() any {
		buf := make([]byte, 1024)
		return &buf
	}}
	t.Cleanup(func() {
		limiter.Stop()
		masterDataSemaphore, globalRateLimiter, readBufferPool = previousSemaphore, previousLimiter, previousPool
	})
}

func startupReadFreshCache(t *testing.T) {
	t.Helper()
	previousCache := raCache
	raCache = newReadAheadCache()
	t.Cleanup(func() { raCache = previousCache })
}

type startupWakeFake struct {
	mu    sync.Mutex
	errs  []error
	calls []struct {
		magnet string
		index  int
	}
}

func (f *startupWakeFake) wake(magnet string, index int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		magnet string
		index  int
	}{magnet: magnet, index: index})
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *startupWakeFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// startupReadTerminal is the brief's S1/S2 contract: an unavailable engine is a
// terminal I/O result. EAGAIN, ENOENT and success are all wrong.
func startupReadTerminal(errno syscall.Errno) bool {
	return errno == syscall.EIO || errno == syscall.ETIMEDOUT
}

func startupReadNode(path string, size int64, wake func(string, int) error) *VirtualMkvNode {
	return &VirtualMkvNode{vMeta: &vfs.Metadata{Path: path, URL: streamURL(hashA, idxA), Size: size}, wake: wake}
}

// startupReadOpen runs Open on the calling goroutine. A panic means Open reached the
// real GoStorm client (uninitialised in a unit process) instead of the injected
// activation dependency; report it as a failed assertion rather than a crashed binary.
func startupReadOpen(t *testing.T, ctx context.Context, n *VirtualMkvNode) (fs.FileHandle, syscall.Errno) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Open panicked instead of returning an errno; it bypassed the injected activation: %v", r)
		}
	}()
	h, _, errno := n.Open(ctx, 0)
	return h, errno
}

type startupReadState struct {
	handles int
	pumps   int
	slots   int
}

func startupReadSnapshot() startupReadState {
	var s startupReadState
	activeHandles.Range(func(_, _ any) bool { s.handles++; return true })
	activePumps.Range(func(_, _ any) bool { s.pumps++; return true })
	s.slots = len(masterDataSemaphore)
	return s
}

// startupReadRequireNoState asserts, on the real registries, that nothing was
// published for path and that no global count moved relative to before.
func startupReadRequireNoState(t *testing.T, path string, before startupReadState) {
	t.Helper()
	if got := startupReadSnapshot(); got != before {
		t.Errorf("registry counts = %+v, want unchanged %+v (activeHandles/activePumps/master slots leaked)", got, before)
	}
	if _, ok := activePumps.Load(path); ok {
		t.Error("activePumps holds an orphan pump for the path")
	}
	if _, ok := playbackRegistry.Load(path); ok {
		t.Error("playbackRegistry holds playback state for the path")
	}
	if _, ok := tailFillTargets.Load(path); ok {
		t.Error("tailFillTargets holds a tail-fill target for the path")
	}
	if v, ok := sessions.Load(path); ok && !v.(*TTFFSession).closed.Load() {
		t.Error("an open TTFF session was left behind for the path")
	}
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("globalOpenTracker.CountByPath = %d, want 0", got)
	}
	activeHandles.Range(func(k, _ any) bool {
		if h, ok := k.(*MkvHandle); ok && h.path == path {
			t.Error("activeHandles holds a handle for the path")
			return false
		}
		return true
	})
	inFlightFetches.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && strings.HasPrefix(key, path+":") {
			t.Errorf("inFlightFetches still holds %q", key)
		}
		return true
	})
}

func startupReadHandle(path string, size int64) *MkvHandle {
	return &MkvHandle{
		path:             path,
		hash:             hashA,
		fileID:           idxA,
		size:             size,
		lastOff:          -1,
		lastActivityTime: time.Now(),
		lastGlobalUpdate: time.Now(),
		usesWarmup:       pathUsesSSDWarmup(path),
	}
}

// S1, I1, I2: a synchronous Wake failure from an otherwise valid torrent link
// must be an ordinary terminal I/O result for every media section, with the same
// errno for audio and movie/TV. The activation dependency is an in-process fake;
// no torrent, peer, tracker, or FUSE mount is involved.
func TestStartupRead_OpenRejectsUnavailableEngineWithoutPublishingState(t *testing.T) {
	got := map[string]syscall.Errno{}
	for _, tc := range startupReadSections {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			path := e.phys("", tc.path)
			t.Cleanup(func() { startupReadClearPath(path) })
			wake := &startupWakeFake{errs: []error{errors.New("BT client not connected")}}
			before := startupReadSnapshot()

			h, errno := startupReadOpen(t, context.Background(), startupReadNode(path, 1024, wake.wake))
			got[tc.name] = errno
			if !startupReadTerminal(errno) {
				t.Fatalf("Open errno = %v, want EIO or ETIMEDOUT when synchronous Wake cannot start", errno)
			}
			if h != nil {
				t.Error("Open returned a handle after its synchronous Wake failed")
			}
			startupReadRequireNoState(t, path, before)
			if wake.callCount() != 1 {
				t.Fatalf("Wake calls = %d, want 1", wake.callCount())
			}
			if c := wake.calls[0]; c.magnet != "magnet:?xt=urn:btih:"+hashA || c.index != idxA {
				t.Errorf("Wake(%q, %d), want valid magnet for (%s, %d)", c.magnet, c.index, hashA, idxA)
			}
		})
	}
	if got["movie"] != got["audio"] {
		t.Errorf("Open errno differs by section: movie=%v audio=%v, want one shared mapping", got["movie"], got["audio"])
	}
}

// I1: with a resident-looking client (so URL resolution succeeds and a pump *could*
// start), a failed Open must leave no handle, pump, master slot, TTFF session or
// open-tracker count behind. The registries are the real ones.
func TestStartupRead_FailedOpenLeaksNoHandlePumpSlotOrSession(t *testing.T) {
	for _, tc := range startupReadSections {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			startupReadFreshCache(t)
			path := e.phys("", tc.path)
			t.Cleanup(func() { startupReadClearPath(path) })
			nativeBridge = native.NewNativeClient() // resolveTargetFile now succeeds from the URL alone
			wake := &startupWakeFake{errs: []error{errors.New("BT client not connected")}}
			before := startupReadSnapshot()

			h, errno := startupReadOpen(t, context.Background(), startupReadNode(path, 1024, wake.wake))
			if !startupReadTerminal(errno) || h != nil {
				t.Fatalf("Open = (handle %T, errno %v), want (nil, EIO|ETIMEDOUT)", h, errno)
			}
			startupReadRequireNoState(t, path, before)
			if wake.callCount() != 1 {
				t.Errorf("Wake calls = %d, want the injected activation used exactly once", wake.callCount())
			}
		})
	}
}

// E1: an already cancelled kernel request must win over the bounded Open startup
// work, for every section, without relying on elapsed-time assertions.
func TestStartupRead_OpenCancellationReturnsEINTR(t *testing.T) {
	for _, tc := range startupReadSections {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			path := e.phys("", tc.path)
			t.Cleanup(func() { startupReadClearPath(path) })
			wake := &startupWakeFake{errs: []error{errors.New("BT client not connected")}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before := startupReadSnapshot()

			h, errno := startupReadOpen(t, ctx, startupReadNode(path, 1024, wake.wake))
			if errno != syscall.EINTR {
				t.Fatalf("cancelled Open errno = %v, want EINTR", errno)
			}
			if h != nil {
				t.Error("cancelled Open returned a handle")
			}
			startupReadRequireNoState(t, path, before)
		})
	}
}

// E1: cancelling while the synchronous Wake is still running must release Open
// with EINTR at once. The Wake is held on a channel the test never closes until
// cleanup, so an Open that waits out the activation (or any retry budget) hangs.
func TestStartupRead_OpenCancelledWhileWakeBlockedReturnsEINTR(t *testing.T) {
	for _, tc := range startupReadSections {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			path := e.phys("", tc.path)
			started, release := make(chan struct{}), make(chan struct{})
			var startOnce sync.Once
			wake := func(string, int) error {
				startOnce.Do(func() { close(started) })
				<-release
				return errors.New("BT client not connected")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := startupReadSnapshot()

			type result struct {
				h     fs.FileHandle
				errno syscall.Errno
			}
			done, exited := make(chan result, 1), make(chan struct{})
			t.Cleanup(func() {
				close(release)
				<-exited
				startupReadClearPath(path)
			})
			go func() {
				defer close(exited)
				defer func() {
					if r := recover(); r != nil {
						done <- result{errno: syscall.ENOTRECOVERABLE}
					}
				}()
				h, _, errno := startupReadNode(path, 1024, wake).Open(ctx, 0)
				done <- result{h, errno}
			}()

			failSafe, stop := context.WithTimeout(context.Background(), startupReadFailSafe)
			defer stop()
			select {
			case <-started:
			case r := <-done:
				t.Fatalf("Open returned (handle %T, errno %v) without ever running the activation", r.h, r.errno)
			case <-failSafe.Done():
				t.Fatal("Open never started the synchronous activation")
			}
			cancel()
			select {
			case r := <-done:
				if r.errno != syscall.EINTR {
					t.Errorf("Open errno = %v, want EINTR after cancellation", r.errno)
				}
				if r.h != nil {
					t.Error("Open returned a handle after cancellation")
				}
				startupReadRequireNoState(t, path, before)
			case <-failSafe.Done():
				t.Fatal("Open stayed blocked on Wake after its caller was cancelled")
			}
		})
	}
}

// I3: publishing an audio projection and resolving it after a restart (a fresh node
// tree and cold metadata cache) is lazy while the BT client is unavailable, exactly
// like movie/TV. Namespace publication and lookup must not wait for the engine
// and must leave no activation, pump, slot or session behind for the projection.
// A hydration attempt on the uninitialised GoStorm client panics or blocks, both of
// which surface here as a failed assertion.
func TestStartupRead_AudioPublicationAndLookupStayLazyWhileEngineUnavailable(t *testing.T) {
	e := newVFSEnv(t)
	startupReadConfigureReaderGlobals(t)
	const rel = "Artist/Lazy_01234567.flac"
	path := e.phys("", "music/"+rel)
	t.Cleanup(func() { startupReadClearPath(path) })
	writeFile(t, path, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
	e.publish(committedA(rel))
	nativeBridge = native.NewNativeClient()
	before := startupReadSnapshot()

	resolveAll := func(step string) {
		t.Helper()
		dir := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupCancellable(c, "music/Artist") }).
			mustReturn(t, step+": audio parent Lookup")
		if dir.status != 0 {
			t.Fatalf("%s: audio parent Lookup status = %v, want OK while BT is unavailable", step, dir.status)
		}
		if st := startCall(t, func(c <-chan struct{}) int { return int(e.readdirViaBridge(c, dir.nodeID)) }).
			mustReturn(t, step+": audio Readdir"); st != 0 {
			t.Fatalf("%s: audio Readdir status = %v, want OK while BT is unavailable", step, st)
		}
		res := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupCancellable(c, "music/"+rel) }).
			mustReturn(t, step+": audio Lookup")
		if res.status != 0 {
			t.Fatalf("%s: audio Lookup status = %v, want OK while BT is unavailable", step, res.status)
		}
	}

	resolveAll("first boot")
	e.remount()
	e.evictMetadataCache()
	resolveAll("after restart")

	startupReadRequireNoState(t, path, before)
}

// S2, I2: failed demand fetches are terminal I/O errors for both sections. In
// particular EAGAIN and a zero-byte or short success would ask the kernel to retry
// or cache a hole before EOF.
func TestStartupRead_UnavailableEngineReadReturnsEIO(t *testing.T) {
	const size = 4096
	cases := []struct {
		name string
		off  int64
		dest int
	}{
		{name: "head", off: 0, dest: 512},
		{name: "mid-file", off: 1024, dest: 512},
		{name: "final partial block returns an error, not a short success", off: size - 100, dest: 512},
	}
	got := map[string]syscall.Errno{}
	for _, sec := range startupReadSections {
		for _, tc := range cases {
			t.Run(sec.name+"/"+tc.name, func(t *testing.T) {
				e := newVFSEnv(t)
				startupReadConfigureReaderGlobals(t)
				startupReadFreshCache(t)
				path := e.phys("", sec.path)
				t.Cleanup(func() { startupReadClearPath(path) })
				nativeBridge = native.NewNativeClient() // no resident torrent for this hash
				before := startupReadSnapshot()

				h := startupReadHandle(path, size)
				res, errno := h.Read(context.Background(), make([]byte, tc.dest), tc.off)
				got[sec.name+"/"+tc.name] = errno
				if !startupReadTerminal(errno) {
					t.Errorf("Read errno = %v, want EIO or ETIMEDOUT after the bounded unavailable-engine retries", errno)
				}
				if res != nil && res.Size() != 0 {
					t.Errorf("Read returned %d bytes with the engine unavailable, want no successful short result", res.Size())
				}
				startupReadRequireNoState(t, path, before)
			})
		}
	}
	for _, tc := range cases {
		if got["movie/"+tc.name] != got["audio/"+tc.name] {
			t.Errorf("%s: Read errno differs by section: movie=%v audio=%v, want one shared mapping", tc.name, got["movie/"+tc.name], got["audio/"+tc.name])
		}
	}
}

// S2 boundary: the terminal-error rule must not turn legitimate EOF into an error.
// A read at or past the end is an empty success and never needs the engine.
func TestStartupRead_ReadAtEOFIsEmptySuccessWithoutEngine(t *testing.T) {
	e := newVFSEnv(t)
	startupReadConfigureReaderGlobals(t)
	startupReadFreshCache(t)
	path := e.phys("", "movies/Eof_01234567.mkv")
	t.Cleanup(func() { startupReadClearPath(path) })
	nativeBridge = native.NewNativeClient()

	res, errno := startupReadHandle(path, 4096).Read(context.Background(), make([]byte, 512), 4096)
	if errno != 0 {
		t.Fatalf("Read at EOF errno = %v, want OK", errno)
	}
	if res == nil || res.Size() != 0 {
		t.Errorf("Read at EOF = %v, want an empty result", res)
	}
}

// S3: a failed Read must not cache the failure. Bytes that later become available
// (a valid warm cache entry standing in for a recovered engine) are served by the
// same handle and by a fresh one, without contacting the engine.
func TestStartupRead_FailedReadDoesNotPoisonLaterCacheHit(t *testing.T) {
	for _, sec := range startupReadSections {
		t.Run(sec.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			startupReadFreshCache(t)
			path := e.phys("", sec.path)
			t.Cleanup(func() { startupReadClearPath(path) })
			nativeBridge = native.NewNativeClient()

			h := startupReadHandle(path, 4096)
			if _, errno := h.Read(context.Background(), make([]byte, 4), 0); !startupReadTerminal(errno) {
				t.Fatalf("first Read errno = %v, want EIO or ETIMEDOUT", errno)
			}
			if raCache.Exists(path, 0) {
				t.Error("failed Read left a negative or partial entry in the read-ahead cache")
			}

			raCache.Put(path, 0, 3, []byte("warm"))
			for name, reader := range map[string]*MkvHandle{"same handle": h, "fresh handle": startupReadHandle(path, 4096)} {
				res, errno := reader.Read(context.Background(), make([]byte, 4), 0)
				if errno != 0 {
					t.Fatalf("%s: warm-cache Read errno = %v, want OK", name, errno)
				}
				data, status := res.Bytes(nil)
				if status != 0 || string(data) != "warm" {
					t.Errorf("%s: warm-cache Read = (%q, %v), want (%q, OK)", name, data, status, "warm")
				}
			}
		})
	}
}

// E1: Read checks the caller cancellation between its bounded fetch attempts and
// must not burn the retry budget or leave a fetch flight / master slot behind.
func TestStartupRead_CancelledReadReturnsEINTR(t *testing.T) {
	for _, sec := range startupReadSections {
		t.Run(sec.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			startupReadFreshCache(t)
			path := e.phys("", sec.path)
			t.Cleanup(func() { startupReadClearPath(path) })
			nativeBridge = native.NewNativeClient()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before := startupReadSnapshot()

			res, errno := startupReadHandle(path, 4096).Read(ctx, make([]byte, 512), 0)
			if errno != syscall.EINTR {
				t.Fatalf("cancelled Read errno = %v, want EINTR", errno)
			}
			if res != nil {
				t.Errorf("cancelled Read returned a result of %d bytes", res.Size())
			}
			startupReadRequireNoState(t, path, before)
		})
	}
}

// E1: a Read queued behind a saturated master-slot pool is released by cancellation
// instead of waiting out the pool's own timeout. The pool is filled by the test and
// only the cancel can unblock the Read.
func TestStartupRead_CancelledWhileWaitingForSlotReturnsEINTR(t *testing.T) {
	e := newVFSEnv(t)
	startupReadConfigureReaderGlobals(t)
	startupReadFreshCache(t)
	path := e.phys("", "movies/Queued_01234567.mkv")
	t.Cleanup(func() { startupReadClearPath(path) })
	nativeBridge = native.NewNativeClient()
	masterDataSemaphore <- struct{}{} // the single slot is taken by someone else
	before := startupReadSnapshot()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		res   int
		errno syscall.Errno
	}
	done := make(chan result, 1)
	go func() {
		res, errno := startupReadHandle(path, 4096).Read(ctx, make([]byte, 512), 0)
		n := -1
		if res != nil {
			n = res.Size()
		}
		done <- result{n, errno}
	}()
	cancel()

	failSafe, stop := context.WithTimeout(context.Background(), startupReadFailSafe)
	defer stop()
	select {
	case r := <-done:
		if r.errno != syscall.EINTR || r.res != -1 {
			t.Errorf("Read = (%d bytes, errno %v), want (no result, EINTR)", r.res, r.errno)
		}
	case <-failSafe.Done():
		t.Fatal("Read stayed queued for a master slot after its caller was cancelled")
	}
	startupReadRequireNoState(t, path, before)
}

// S3, I2: a rejected cold Open, repeated, must not poison a later independent Open
// in either section. Every attempt calls activation again; once it recovers the new
// Open succeeds and a valid warm cache serves bytes without any engine call.
func TestStartupRead_FailedOpenDoesNotPoisonLaterWarmCacheRead(t *testing.T) {
	for _, sec := range startupReadSections {
		t.Run(sec.name, func(t *testing.T) {
			e := newVFSEnv(t)
			startupReadConfigureReaderGlobals(t)
			startupReadFreshCache(t)
			path := e.phys("", sec.path)
			t.Cleanup(func() { startupReadClearPath(path) })

			down := errors.New("BT client not connected")
			wake := &startupWakeFake{errs: []error{down, down, nil}}
			before := startupReadSnapshot()
			for attempt := 1; attempt <= 2; attempt++ {
				h, errno := startupReadOpen(t, context.Background(), startupReadNode(path, 4, wake.wake))
				if !startupReadTerminal(errno) || h != nil {
					t.Fatalf("unavailable Open #%d = (handle %T, errno %v), want (nil, EIO|ETIMEDOUT)", attempt, h, errno)
				}
				startupReadRequireNoState(t, path, before)
			}

			raCache.Put(path, 0, 3, []byte("warm"))
			rawHandle, errno := startupReadOpen(t, context.Background(), startupReadNode(path, 4, wake.wake))
			if errno != 0 {
				t.Fatalf("independent Open after recovery errno = %v, want OK", errno)
			}
			h, ok := rawHandle.(*MkvHandle)
			if !ok {
				t.Fatalf("independent Open handle = %T, want *MkvHandle", rawHandle)
			}
			h.lastGlobalUpdate = time.Now() // this harness has no global cleanup manager to notify
			if wake.callCount() != 3 {
				t.Fatalf("Wake calls after recovery = %d, want 3 independent attempts (no cached failure)", wake.callCount())
			}

			result, errno := h.Read(context.Background(), make([]byte, 4), 0)
			if errno != 0 {
				t.Fatalf("warm-cache Read errno = %v, want OK", errno)
			}
			gotBytes, status := result.Bytes(nil)
			if status != 0 || string(gotBytes) != "warm" {
				t.Errorf("warm-cache Read = (%q, %v), want (%q, OK)", gotBytes, status, "warm")
			}
			if wake.callCount() != 3 {
				t.Errorf("warm-cache Read contacted the engine: Wake calls = %d, want still 3", wake.callCount())
			}
		})
	}
}
