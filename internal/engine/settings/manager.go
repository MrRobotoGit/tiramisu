package settings

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/metadb"
)

// The settings row of gostorm_kv, the same one today's binary uses.
const (
	kvXPath = "Settings"
	kvName  = "BitTorr"
	// Unreadable rows and the import marker live here, outside the xpath today's binary reads.
	metaXPath = "Meta"
)

// ErrNeedsReconnect is returned by ApplyLive when the engine runs settings that only a
// reconnect can bring in line; the Manager then reconnects.
var ErrNeedsReconnect = errors.New("settings: the engine needs a reconnect")

// Applier puts saved settings in use on the running engine.
type Applier interface {
	ApplyLive(Settings) error // only KindLive keys changed: torrents are not touched
	Reconnect(Settings) error // new client and piece store: every torrent is dropped
	InUse(Settings) bool      // the engine is up and runs these settings
}

// Options configure a Manager.
type Options struct {
	DB          *metadb.DB
	PortManaged bool // NAT-PMP owns PeersListenPort: not compared, never restored
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

type state struct {
	s   Settings
	raw document // the stored keys, uninterpreted ones included
	doc []byte   // raw overlaid with s: what Document returns
}

// Manager owns the settings document. Reads are lock-free snapshots; writes, history and
// apply are serialised by one mutex that the engine never takes (V227).
type Manager struct {
	o       Options
	hist    history
	mu      sync.Mutex
	applier Applier // guarded by mu
	applied bool    // guarded by mu: the stored settings are the ones the engine runs
	cur     atomic.Pointer[state]
}

// Open loads the stored document, or writes the defaults when there is none, then records
// the history baseline when the history is empty. A DB error is fatal for the caller.
func Open(o Options) (*Manager, error) {
	if o.DB == nil {
		return nil, errors.New("settings: no state DB")
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Manager{o: o, hist: history{db: o.DB, now: o.Now}}
	b, err := o.DB.GoStormKVGet(kvXPath, kvName)
	if err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	var st *state
	if b != nil {
		raw, s, perr := parseDocument(b)
		if perr == nil {
			s = s.loaded()
			doc, err := raw.with(s)
			if err != nil {
				return nil, err
			}
			st = &state{s: s, raw: raw, doc: doc}
		} else {
			name := "settings_corrupt-" + o.Now().Format("20060102-150405")
			if err := o.DB.GoStormKVSet(metaXPath, name, b); err != nil {
				return nil, fmt.Errorf("settings: quarantining an unreadable document: %w", err)
			}
			o.Logf("[Settings] WARN stored document unreadable (%v): kept as %s/%s, defaults in use", perr, metaXPath, name)
		}
	}
	if st == nil {
		raw, s := document{}, Defaults()
		doc, err := raw.with(s)
		if err != nil {
			return nil, err
		}
		if err := o.DB.GoStormKVSet(kvXPath, kvName, doc); err != nil {
			return nil, fmt.Errorf("settings: writing defaults: %w", err)
		}
		st = &state{s: s, raw: raw, doc: doc}
		o.Logf("[Settings] no stored document: defaults written")
	}
	m.cur.Store(st)
	if _, _, err := m.hist.record(st.doc, "default", "", "baseline", m.IgnoredKeys(), true); err != nil {
		o.Logf("[Settings] WARN history baseline: %v", err)
	}
	return m, nil
}

// SetApplier connects the engine; before it, saves are stored but not applied. An engine
// that does not run the settings in use (a port written after engine.New) is reconnected.
func (m *Manager) SetApplier(a Applier) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applier = a
	cur := m.cur.Load().s
	if m.applied = a.InUse(cur); m.applied {
		return nil
	}
	var res Result
	err := m.apply(ApplyReconnect, cur, &res)
	m.o.Logf("[Settings] engine behind the stored settings: %s", res.Apply)
	return err
}

// Current is the settings in use, as an immutable value.
func (m *Manager) Current() Settings { return m.cur.Load().s }

// Document is the settings in use as the stored JSON, uninterpreted keys included.
func (m *Manager) Document() []byte { return bytes.Clone(m.cur.Load().doc) }

// IgnoredKeys are the keys the history neither compares nor restores.
func (m *Manager) IgnoredKeys() []string { return ignoredKeys(m.o.PortManaged) }
