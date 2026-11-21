package streamer

// Aggressive returns the last SetAggressive request, or the one a new stream turned on.
func (t *Torrent) Aggressive() (on bool, masterLimit int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.aggrOn, t.aggrLimit
}
