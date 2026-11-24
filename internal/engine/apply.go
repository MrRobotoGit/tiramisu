package engine

import "errors"

// ErrNeedsReconnect: the change touches a client option or the piece store.
var ErrNeedsReconnect = errors.New("engine: settings change needs a reconnect")

// LiveOnly reports whether a and b differ only in fields read on every use: the expiry
// timeout, RetrackersMode (new torrents and fan-out), ResponsiveMode and AdaptiveShield.
func LiveOnly(a, b Config) bool {
	a, b = a.normalized(), b.normalized()
	a.TorrentDisconnectTimeout, a.RetrackersMode = b.TorrentDisconnectTimeout, b.RetrackersMode
	a.ResponsiveMode, a.AdaptiveShield = b.ResponsiveMode, b.AdaptiveShield
	a.FriendsMaxHelps = b.FriendsMaxHelps
	return a == b
}

// SameConfig reports whether a and b are the same configuration once clamped.
func SameConfig(a, b Config) bool { return a.normalized() == b.normalized() }

// ApplyLive puts cfg in use without touching the client or any torrent. It refuses a
// change that only a Reconnect can apply.
func (e *Engine) ApplyLive(cfg Config) error {
	e.setMu.Lock()
	defer e.setMu.Unlock()
	if !LiveOnly(e.config(), cfg) {
		return ErrNeedsReconnect
	}
	c := cfg.normalized()
	e.cfg.Store(&c)
	e.deps.logf("[Tentacle] settings applied live (timeout %ds, retrackers %d, responsive %v, shield %v)",
		c.TorrentDisconnectTimeout, c.RetrackersMode, c.ResponsiveMode, c.AdaptiveShield)
	return nil
}
