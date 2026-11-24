package engine

import (
	"sync"
)

// ScanSlotLimit is how many data slots background consumers may hold: few while a confirmed
// playback runs, so scans and friends never take what the stream needs. The floor of 1 lets an
// idle semaphore always grant one.
func ScanSlotLimit(capacity int, ownPlayback bool) int {
	if ownPlayback {
		return 5
	}
	if n := capacity - 5; n > 1 {
		return n
	}
	return 1
}

type background struct {
	slots       chan struct{}
	ownPlayback func() bool
}

// SetBackground shares the FUSE data slots with the engine's background work (friends).
func (e *Engine) SetBackground(slots chan struct{}, ownPlayback func() bool) {
	if slots == nil || ownPlayback == nil {
		e.bg.Store(nil)
		return
	}
	e.bg.Store(&background{slots: slots, ownPlayback: ownPlayback})
}

// takeBackgroundSlot takes a slot without waiting, under ScanSlotLimit; ok=false when full. A slot
// is never revoked: only its release frees it.
func (e *Engine) takeBackgroundSlot() (release func(), ok bool) {
	b := e.bg.Load()
	if b == nil || len(b.slots) >= ScanSlotLimit(cap(b.slots), b.ownPlayback()) {
		return func() {}, false
	}
	select {
	case b.slots <- struct{}{}:
	default:
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { <-b.slots }) }, true
}
