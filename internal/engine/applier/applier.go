// Package applier binds the settings document to the engine: engine never imports settings.
package applier

import (
	"errors"
	"fmt"

	"tiramisu/internal/engine"
	"tiramisu/internal/engine/settings"
)

// Config maps the settings onto the engine's Config. ReaderReadAHead, UseDisk and
// TorrentsSavePath are not engine knobs: the streamer and the warmup read the snapshot.
func Config(s settings.Settings) engine.Config {
	return engine.Config{
		CacheSize: s.CacheSize, ConnectionsLimit: s.ConnectionsLimit,
		TorrentDisconnectTimeout: s.TorrentDisconnectTimeout, RetrackersMode: s.RetrackersMode,
		EnableDebug: s.EnableDebug, EnableIPv6: s.EnableIPv6, DisableTCP: s.DisableTCP,
		DisableUTP: s.DisableUTP, DisableDHT: s.DisableDHT, DisablePEX: s.DisablePEX,
		DisableUpload: s.DisableUpload, ForceEncrypt: s.ForceEncrypt,
		AggressivePeerManagement: s.AggressivePeerManagement,
		DownloadRateLimit:        s.DownloadRateLimit, UploadRateLimit: s.UploadRateLimit,
		PeersListenPort: s.PeersListenPort, DisableUPNP: s.DisableUPNP, ResponsiveMode: s.ResponsiveMode, AdaptiveShield: s.AdaptiveShield,
		FriendsMaxHelps: s.FriendsMaxHelps,
	}
}

// Target is the engine side of an apply; *engine.Engine implements it.
type Target interface {
	ApplyLive(engine.Config) error
	Reconnect(engine.Config) error
	Up() bool
	Config() engine.Config
}

type applier struct{ t Target }

// New returns the settings.Applier for t.
func New(t Target) settings.Applier { return applier{t} }

// ApplyLive maps the engine's refusal onto the settings error, so the Manager reconnects.
func (a applier) ApplyLive(s settings.Settings) error {
	err := a.t.ApplyLive(Config(s))
	if errors.Is(err, engine.ErrNeedsReconnect) {
		return fmt.Errorf("%w: %w", settings.ErrNeedsReconnect, err)
	}
	return err
}

func (a applier) Reconnect(s settings.Settings) error { return a.t.Reconnect(Config(s)) }

func (a applier) InUse(s settings.Settings) bool {
	return a.t.Up() && engine.SameConfig(a.t.Config(), Config(s))
}

var _ Target = (*engine.Engine)(nil)
