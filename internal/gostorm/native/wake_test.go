package native

import (
	"context"
	"errors"
	"testing"
	"time"
)

const invalidWakeLink = "bogus://not-a-torrent"

// M10: a cancelled Wake returns promptly and leaves no semaphore token behind. The FUSE
// Open that called it is already gone, and a token held to the metadata timeout would
// exhaust the semaphore after a handful of cancelled cold opens.
func TestWakeCancelledContextLeavesNoToken_M10(t *testing.T) {
	c := NewNativeClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- c.Wake(ctx, invalidWakeLink, 1) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wake() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wake() did not return promptly on a cancelled context")
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("semaphore tokens held after a cancelled Wake = %d, want 0", got)
	}
}

// A Wake that fails before adding anything must still release the token it took.
func TestWakeReleasesTokenOnFailure_M10(t *testing.T) {
	c := NewNativeClient()
	if err := c.Wake(context.Background(), invalidWakeLink, 1); err == nil {
		t.Fatal("Wake() error = nil, want a parse failure")
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("semaphore tokens held after a failed Wake = %d, want 0", got)
	}
}
