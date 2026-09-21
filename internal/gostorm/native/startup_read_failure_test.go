package native

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startupReadFailSafe only bounds how long a test waits for something a correct
// implementation completes immediately or at the injected deadline. It never
// substitutes for synchronization: every passing path is released by a channel.
const startupReadFailSafe = 2 * time.Second

// startupReadStubStream replaces the torrent-backed stream. The fake never closes
// the pipe writer by itself, exactly like a backing stream that stalls: only
// FetchAhead's own deadline handling can unblock its reader. Writers are closed at
// cleanup so nothing outlives the test.
func startupReadStubStream(t *testing.T, timeout time.Duration, payload []byte) (deadlineObserved <-chan struct{}) {
	t.Helper()
	oldStreamRange, oldTimeout := streamRangeFn, fetchBlockTimeout
	t.Cleanup(func() {
		streamRangeFn = oldStreamRange
		fetchBlockTimeout = oldTimeout
	})
	observed := make(chan struct{})
	var mu sync.Mutex
	var writers []*io.PipeWriter
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, pw := range writers {
			_ = pw.Close()
		}
	})
	streamRangeFn = func(ctx context.Context, _ string, _ int, _ int64, _ int64, pw *io.PipeWriter) error {
		mu.Lock()
		writers = append(writers, pw)
		mu.Unlock()
		go func() {
			if len(payload) > 0 {
				_, _ = pw.Write(payload)
			}
			<-ctx.Done()
			close(observed)
		}()
		return nil
	}
	fetchBlockTimeout = timeout
	return observed
}

type startupReadFetchResult struct {
	n   int
	err error
}

// startupReadFetchAhead runs FetchAhead off the test goroutine so that a missing
// deadline is a failed assertion (via the fail-safe) rather than a hung test binary.
func startupReadFetchAhead(t *testing.T, buf, dest []byte) (ret <-chan startupReadFetchResult, fills <-chan error, callbacks *atomic.Int32) {
	t.Helper()
	retCh, fillCh := make(chan startupReadFetchResult, 1), make(chan error, 4)
	callbacks = new(atomic.Int32)
	go func() {
		n, err := NewNativeClient().FetchAhead("absent", 0, 0, buf, dest, func(_ int, complete bool, fillErr error) {
			if !complete {
				return
			}
			callbacks.Add(1)
			fillCh <- fillErr
		})
		retCh <- startupReadFetchResult{n, err}
	}()
	return retCh, fillCh, callbacks
}

// E2: FetchAhead must end a backing stream that has neither produced bytes nor
// closed itself. The injected stream observes the test-controlled deadline and,
// like a real stall, leaves the pipe open: only FetchAhead's own handling of that
// deadline can release the reader. The test never sleeps or relies on a network
// timeout.
func TestStartupRead_FetchAheadStalledStreamEndsAtInjectedDeadline(t *testing.T) {
	deadlineObserved := startupReadStubStream(t, time.Millisecond, nil)
	ret, fills, callbacks := startupReadFetchAhead(t, make([]byte, 8), make([]byte, 4))

	failSafe, stop := context.WithTimeout(context.Background(), startupReadFailSafe)
	defer stop()
	select {
	case r := <-ret:
		if r.n != 0 {
			t.Errorf("FetchAhead byte count = %d, want 0 for a stalled stream", r.n)
		}
		if r.err == nil {
			t.Error("FetchAhead error = nil, want terminal deadline error")
		}
	case <-failSafe.Done():
		t.Fatal("FetchAhead stayed blocked on a stalled stream past its injected deadline")
	}
	select {
	case <-deadlineObserved:
	case <-failSafe.Done():
		t.Fatal("FetchAhead returned before its injected stream deadline fired")
	}
	select {
	case fillErr := <-fills:
		if fillErr == nil {
			t.Error("terminal fill callback error = nil, want deadline error")
		}
	case <-failSafe.Done():
		t.Fatal("terminal fill callback did not arrive after FetchAhead ended")
	}
	if got := callbacks.Load(); got != 1 {
		t.Errorf("terminal fill callbacks = %d, want exactly 1", got)
	}
}

// E2: a stream that yields the head bytes and then stalls must not wedge the
// background fill. The caller gets its head bytes, and the terminal callback that
// recycles the buffer and releases reads queued behind it still arrives once the
// injected deadline passes.
func TestStartupRead_FetchAheadStallAfterHeadStillCompletesFill(t *testing.T) {
	deadlineObserved := startupReadStubStream(t, time.Millisecond, []byte("abcd"))
	dest := make([]byte, 4)
	ret, fills, callbacks := startupReadFetchAhead(t, make([]byte, 8), dest)

	failSafe, stop := context.WithTimeout(context.Background(), startupReadFailSafe)
	defer stop()
	select {
	case r := <-ret:
		if r.err != nil || r.n != 4 || string(dest) != "abcd" {
			t.Errorf("FetchAhead = (%d, %v, %q), want the 4 head bytes and no error", r.n, r.err, dest)
		}
	case <-failSafe.Done():
		t.Fatal("FetchAhead did not return the head bytes it had already received")
	}
	select {
	case <-fills:
	case <-failSafe.Done():
		t.Fatal("terminal fill callback never arrived for a stream that stalled after the head")
	}
	select {
	case <-deadlineObserved:
	case <-failSafe.Done():
		t.Fatal("the fill finished without the injected deadline firing")
	}
	if got := callbacks.Load(); got != 1 {
		t.Errorf("terminal fill callbacks = %d, want exactly 1", got)
	}
}

// E2 / S2: an absent backing stream (no such torrent) is an immediate terminal
// failure. The deadline is set far beyond the test's fail-safe, so any implementation
// that waits for it before reporting the failure is caught.
func TestStartupRead_FetchAheadAbsentStreamFailsWithoutWaitingForDeadline(t *testing.T) {
	oldStreamRange, oldTimeout := streamRangeFn, fetchBlockTimeout
	t.Cleanup(func() {
		streamRangeFn = oldStreamRange
		fetchBlockTimeout = oldTimeout
	})
	absent := errors.New("torrent not found")
	streamRangeFn = func(context.Context, string, int, int64, int64, *io.PipeWriter) error { return absent }
	fetchBlockTimeout = time.Hour

	ret, fills, callbacks := startupReadFetchAhead(t, make([]byte, 8), make([]byte, 4))

	failSafe, stop := context.WithTimeout(context.Background(), startupReadFailSafe)
	defer stop()
	select {
	case r := <-ret:
		if r.n != 0 || !errors.Is(r.err, absent) {
			t.Errorf("FetchAhead = (%d, %v), want (0, %v)", r.n, r.err, absent)
		}
	case <-failSafe.Done():
		t.Fatal("FetchAhead waited on its deadline for a stream that never existed")
	}
	select {
	case fillErr := <-fills:
		if !errors.Is(fillErr, absent) {
			t.Errorf("terminal fill callback error = %v, want %v", fillErr, absent)
		}
	case <-failSafe.Done():
		t.Fatal("terminal fill callback did not arrive for an absent stream")
	}
	if got := callbacks.Load(); got != 1 {
		t.Errorf("terminal fill callbacks = %d, want exactly 1", got)
	}
}
