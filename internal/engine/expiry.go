package engine

// expired is the expiry rule: no priority, not waiting for metadata, no consumer, deadline
// passed.
func (e *Engine) expired(t *Torrent) bool {
	if s := t.State(); t.IsPriority.Load() || s == StateGettingInfo || s == StateAdded || s == StateClosed || t.Consumers() > 0 {
		return false
	}
	return e.now().UnixNano() >= t.deadline.Load()
}

// expire force-saves (no debounce: the swarm is fullest now) and closes expired torrents, the
// check, save and close as one step under t.life.
func (e *Engine) expire() {
	for _, t := range e.ListHydrated() {
		t.life.Lock()
		if e.expired(t) {
			e.saveLocked(t)
			e.closeLocked(t)
			e.friendsPlaybackEnd(t.hash)
		}
		t.life.Unlock()
	}
}
