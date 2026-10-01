package vfs

import (
	"context"
	"fmt"
	"time"
)

// StartWake runs a torrent activation in the background and waits for it at most grace. An
// activation that fails within the grace (semaphore exhausted, unparsable link) is returned, so
// Open can fail fast and visibly; a slower one keeps running for the reads that follow, and if it
// fails later the failure is logged instead of discarded.
func StartWake(wake func(context.Context) error, grace time.Duration, logf func(string, ...any)) error {
	done := make(chan error, 1)
	go func() {
		// A panicking activation reports an error instead of taking the process down.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("activation panicked: %v", r)
			}
		}()
		done <- wake(context.Background())
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		go func() {
			if err := <-done; err != nil {
				logf("[VFS] background activation failed: %v", err)
			}
		}()
		return nil
	}
}
