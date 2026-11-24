// Package enginehost starts the in-process engine in the order of engine.md §1.13 and holds
// the glue main needs around it: streamer tunables, the settings endpoint, the history
// hooks, the NAT-PMP port and the warmup hook.
package enginehost

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"tiramisu/internal/bridge"
	"tiramisu/internal/engine"
	"tiramisu/internal/engine/applier"
	"tiramisu/internal/engine/settings"
	"tiramisu/internal/metadb"
	"tiramisu/internal/streamer"
)

// Options of Boot.
type Options struct {
	DB       *metadb.DB
	StateDir string // where config.db, settings.json, trackers.txt and blocklist live (--path)
	// PortManaged: NAT-PMP owns PeersListenPort (not compared, never restored by the history).
	PortManaged      bool
	BlockListEnabled bool
	BlockListFilter  string
	PeerInfo         engine.PeerInfoFunc // GeoIP of the swarm view; nil: none
	// Playing tells the scheduler which torrents are in playback (stall risk); nil: all.
	Playing func(hash string) bool
	Logf    func(format string, args ...any)

	newEngine func(engine.Config, engine.Deps) *engine.Engine // tests: an offline engine
}

// Host is the running engine with its settings, scheduler and FUSE bridge.
type Host struct {
	Engine   *engine.Engine
	Settings *settings.Manager
	Streamer *streamer.Streamer
	Bridge   *bridge.NativeClient
	StateDir string
	logf     func(string, ...any)
}

// Boot imports the legacy state, opens the settings, starts the engine and connects the
// settings to it. Every error is fatal for the caller, except a failed apply of the stored
// settings, which is logged: the engine runs and the next save retries it.
func Boot(o Options) (*Host, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := settings.ImportLegacy(o.DB, o.StateDir, logf); err != nil {
		return nil, err
	}
	m, err := settings.Open(settings.Options{DB: o.DB, PortManaged: o.PortManaged, Logf: logf})
	if err != nil {
		return nil, err
	}
	newEngine := o.newEngine
	if newEngine == nil {
		newEngine = engine.New
	}
	e := newEngine(applier.Config(m.Current()), engine.Deps{DB: o.DB, StateDir: o.StateDir, Logf: logf, NATPMP: o.PortManaged})
	if err := e.SetBlockListFilter(o.BlockListFilter); err != nil {
		logf("[BlockList] %v — loading the list unfiltered", err)
	}
	e.SetBlockListEnabled(o.BlockListEnabled) // before Start: the first client reads it
	if o.PeerInfo != nil {
		e.SetPeerInfo(o.PeerInfo)
	}
	if err := e.Start(); err != nil {
		return nil, err
	}
	if err := m.SetApplier(applier.New(e)); err != nil {
		logf("[Settings] WARN the stored settings did not apply: %v", err)
	}
	st := streamer.New(streamer.Config{Logf: logf, Playing: o.Playing, Tunables: func() streamer.Tunables { return Tunables(m.Current()) }})
	e.SetStreamer(st)
	return &Host{Engine: e, Settings: m, Streamer: st, Bridge: bridge.NewNativeClient(e, st), StateDir: o.StateDir, logf: logf}, nil
}

// Close stops the scheduler and the engine (tests; the service exits without it).
func (h *Host) Close() {
	h.Streamer.Close()
	h.Engine.Close()
}

// Tunables maps the settings in use onto the streamer, with the save clamps.
func Tunables(s settings.Settings) streamer.Tunables {
	t := streamer.Tunables{CacheSize: s.CacheSize, ReadAheadPct: s.ReaderReadAHead, ConnectionsLimit: s.ConnectionsLimit,
		IdleTimeout: time.Duration(s.TorrentDisconnectTimeout) * time.Second}
	if t.CacheSize <= 0 {
		t.CacheSize = settings.Defaults().CacheSize
	}
	if t.ReadAheadPct < 5 {
		t.ReadAheadPct = 5
	}
	if t.ConnectionsLimit <= 0 {
		t.ConnectionsLimit = settings.Defaults().ConnectionsLimit
	}
	if t.IdleTimeout <= 0 {
		t.IdleTimeout = 30 * time.Second
	}
	return t
}

// WarmupActive is the SSD warmup's state hook: the fork's warmup hedging follows the file.
func (h *Host) WarmupActive(hash string, fileID int, active bool) {
	if t := h.Engine.Get(hash); t != nil {
		t.SetWarmupActive(active, t.EngineFileIndex(fileID))
	}
}

// PeerPort is the peer listen port as NAT-PMP manages it.
func (h *Host) PeerPort() PeerPort { return PeerPort{h.Settings} }

type PeerPort struct{ m *settings.Manager }

func (p PeerPort) Port() int { return p.m.Current().PeersListenPort }

// SetPort stores the port (never recorded in the history) and reconnects the client.
func (p PeerPort) SetPort(port int) error {
	res, err := p.m.SetPeersListenPort(port)
	if err == nil && res.Err != "" {
		err = fmt.Errorf("%s", res.Err)
	}
	return err
}

// Restore is the history rollback: the interpreted keys it changed, per the settings.
func (h *Host) Restore(doc []byte, actor, note string) ([]string, error) {
	res, err := h.Settings.Restore(doc, actor, note)
	return res.Changed, err
}

// SettingsHandler serves POST /api/tentacle/settings with the body of the retired :8090
// /settings: get returns the document; set and def answer 200 with no body. who names the
// signed-in source and actor for the history.
func (h *Host) SettingsHandler(who func(*http.Request) (source, actor string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Action string          `json:"action"`
			Sets   json.RawMessage `json:"sets"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var res settings.Result
		var err error
		switch req.Action {
		case "get":
			w.Header().Set("Content-Type", "application/json")
			w.Write(h.Settings.Document())
			return
		case "set":
			if len(req.Sets) == 0 || !json.Valid(req.Sets) || req.Sets[0] != '{' {
				http.Error(w, "sets must be an object", http.StatusBadRequest)
				return
			}
			source, actor := who(r)
			res, err = h.Settings.SaveJSON(req.Sets, source, actor)
		case "def":
			source, actor := who(r)
			res, err = h.Settings.ResetDefaults(source, actor)
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.logf("[Settings] %s saved: changed %v, apply %s %s", req.Action, res.Changed, res.Apply, res.Err)
		w.WriteHeader(http.StatusOK)
	}
}
