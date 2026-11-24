package engine

import (
	"context"
	"net"

	"tiramisu/internal/engine/bt"
)

// NewOffline builds an engine that never leaves loopback: no DHT, no tracker announce, no
// public-IP lookup, no remote tracker list, no blocklist file. For tests above the engine.
func NewOffline(cfg Config, d Deps) *Engine {
	cfg.DisableDHT = true
	e := New(cfg, d)
	e.publicIPs = func(bool) (net.IP, net.IP) { return nil, nil }
	e.freshPublicIPs = func(context.Context, bool) (net.IP, net.IP) { return nil, nil }
	e.tuneBT = func(o *bt.Options) { o.DisableTrackers, o.PortForwarding = true, false }
	e.block.binDir = ""
	e.loader.Stop()
	return e
}
