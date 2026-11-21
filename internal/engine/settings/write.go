package settings

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Apply is what a write did to the running engine.
type Apply int

const (
	ApplyNone      Apply = iota // nothing changed, or only keys read at start
	ApplyLive                   // stored in the engine: playback untouched
	ApplyReconnect              // client and piece store rebuilt: playback interrupted
	ApplyFailed                 // stored, but the engine refused or failed the apply
)

func (a Apply) String() string {
	switch a {
	case ApplyLive:
		return "live"
	case ApplyReconnect:
		return "reconnect"
	case ApplyFailed:
		return "failed"
	}
	return "none"
}

func (a Apply) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// Result describes a write, for the Control Panel.
type Result struct {
	Changed []string `json:"changed"`           // interpreted keys that changed, sorted
	Apply   Apply    `json:"apply"`             // what was done to the running engine
	Restart []string `json:"restart,omitempty"` // changed keys that take effect at the next start
	Version int64    `json:"version,omitempty"` // history row, 0 when not recorded
	Err     string   `json:"error,omitempty"`   // why the apply failed
}

// needs is the strongest apply the keys require, and the keys that need a restart.
func needs(keys []string) (Apply, []string) {
	a, restart := ApplyNone, []string(nil)
	for _, k := range keys {
		switch kinds[k] {
		case KindLive:
			a = max(a, ApplyLive)
		case KindReconnect, KindStore:
			a = ApplyReconnect
		case KindRestart:
			restart = append(restart, k)
		}
	}
	return a, restart
}

// Save is a manual save (Control Panel "set"); the caller passes who saved.
func (m *Manager) Save(in Settings, source, actor string) (Result, error) {
	source, actor = NormalizeActor(source, actor)
	return m.write(func(Settings) Settings { return in }, true, source, actor, "")
}

// SaveJSON is Save for a "sets" object: the keys it names overlay the settings in use.
func (m *Manager) SaveJSON(sets []byte, source, actor string) (Result, error) {
	var check Settings
	if err := json.Unmarshal(sets, &check); err != nil {
		return Result{}, fmt.Errorf("settings: %w", err)
	}
	source, actor = NormalizeActor(source, actor)
	return m.write(func(cur Settings) Settings {
		json.Unmarshal(sets, &cur) // validated above; Settings holds scalars only
		return cur
	}, true, source, actor, "")
}

// ResetDefaults is the manual reset ("def").
func (m *Manager) ResetDefaults(source, actor string) (Result, error) {
	source, actor = NormalizeActor(source, actor)
	return m.write(func(Settings) Settings { return Defaults() }, true, source, actor, "defaults")
}

// Restore puts a history version in use, recorded as a rollback.
func (m *Manager) Restore(doc []byte, actor, note string) (Result, error) {
	_, in, err := parseDocument(doc)
	if err != nil {
		return Result{}, fmt.Errorf("settings: version unreadable: %w", err)
	}
	_, actor = NormalizeActor("", actor)
	return m.write(func(Settings) Settings { return in }, true, "rollback", actor, note)
}

// SetPeersListenPort is the NAT-PMP path: automatic, never recorded, port only.
func (m *Manager) SetPeersListenPort(port int) (Result, error) {
	return m.write(func(cur Settings) Settings { cur.PeersListenPort = port; return cur }, false, "", "", "")
}

// write stores, records (manual only) and applies under one lock, in today's order. Settings
// not yet applied (a failed apply) are applied again by any manual write, with a reconnect.
func (m *Manager) write(build func(Settings) Settings, manual bool, source, actor, note string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.cur.Load()
	next := build(cur.s)
	if manual && m.o.PortManaged {
		next.PeersListenPort = cur.s.PeersListenPort
	}
	next = next.clamped()
	res := Result{Changed: changed(cur.s, next)}
	if len(res.Changed) == 0 {
		if !manual || m.applied || m.applier == nil {
			return res, nil
		}
		err := m.apply(ApplyReconnect, cur.s, &res)
		m.o.Logf("[Settings] re-applied unchanged settings: %s", res.Apply)
		return res, err
	}
	doc, err := cur.raw.with(next)
	if err != nil {
		return res, err
	}
	if err := m.o.DB.GoStormKVSet(kvXPath, kvName, doc); err != nil {
		return res, fmt.Errorf("settings: write: %w", err)
	}
	m.cur.Store(&state{s: next, raw: cur.raw, doc: doc})
	if manual {
		id, ok, err := m.hist.record(doc, source, actor, note, m.IgnoredKeys(), false)
		if err != nil {
			m.o.Logf("[Settings] WARN history: %v", err)
		} else if ok {
			res.Version = id
		}
	}
	apply, restart := needs(res.Changed)
	res.Restart = restart
	if !m.applied {
		apply = ApplyReconnect // the engine runs older settings: only a rebuild catches up
	}
	err = m.apply(apply, next, &res)
	m.o.Logf("[Settings] saved %v: %s", res.Changed, res.Apply)
	return res, err
}

// apply hands s to the engine and records whether the stored settings are now in use.
func (m *Manager) apply(a Apply, s Settings, res *Result) error {
	if m.applier == nil {
		return nil
	}
	var err error
	switch a {
	case ApplyLive:
		err = m.applier.ApplyLive(s)
		if errors.Is(err, ErrNeedsReconnect) {
			m.o.Logf("[Settings] live apply refused (%v): reconnecting", err)
			a, err = ApplyReconnect, m.applier.Reconnect(s)
		}
	case ApplyReconnect:
		err = m.applier.Reconnect(s)
	}
	res.Apply = a
	if err != nil {
		res.Apply, res.Err = ApplyFailed, err.Error()
	}
	m.applied = err == nil
	return err
}
