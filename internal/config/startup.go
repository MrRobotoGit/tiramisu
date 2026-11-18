package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tiramisu/internal/metadb"
)

// Boot is what start-up resolves before the configuration exists: where things live,
// then the DB and the store. Nothing here reads gc().
type Boot struct {
	Root, StateDir, DBPath string
	Files                  []string
	DB                     *metadb.DB
	Store                  *Store
}

// Prepare resolves the paths and refuses a config.json that moved the DB elsewhere.
func Prepare(pathFlag string) (*Boot, error) {
	root := RootFrom(pathFlag)
	b := &Boot{Root: root, StateDir: StateDir(root)}
	b.DBPath = DBPath(b.StateDir)
	b.Files = LegacyFiles(root)
	return b, CheckLegacyDBPath(b.Files, b.StateDir)
}

// Open opens the state DB. Failing here is fatal: running on defaults would mean wrong
// paths and ports, so the error says how to get the configuration back.
func (b *Boot) Open(logger metadb.Logger, logf func(string, ...any)) error {
	db, err := metadb.New(b.DBPath, logger)
	if err != nil {
		last := filepath.Join(b.StateDir, "config.last.json")
		return fmt.Errorf("cannot open %s: %v. The configuration has a copy in %s: with the service stopped, move the DB "+
			"aside, run tiramisu --import-config %s, then start Tiramisu (the inode map and sign-in stay in the old DB; "+
			"sqlite3 .recover can rescue them)", b.DBPath, err, last, last)
	}
	b.DB = db
	b.Store = NewStore(db, b.StateDir, logf)
	return nil
}

// Load imports or creates the configuration and returns it in use, with its version.
func (b *Boot) Load() (Config, int64, error) {
	if err := b.Store.Bootstrap(b.Files); err != nil {
		return Config{}, 0, err
	}
	if doc, _, err := b.Store.Current(); err == nil {
		var v struct {
			EnableStateDB *bool `json:"enable_state_db"`
		}
		if json.Unmarshal(doc, &v) == nil && v.EnableStateDB != nil && !*v.EnableStateDB {
			b.Store.logf("[Config] enable_state_db is false: ignored, the state DB holds the configuration and is always on")
		}
	}
	return b.InUse()
}

// InUse resolves the saved document with the environment applied.
func (b *Boot) InUse() (Config, int64, error) {
	doc, ver, err := b.Store.Current()
	if err != nil {
		return Config{}, 0, err
	}
	c, err := Resolve(doc, Env{RootPath: b.Root, LegacyPath: LegacyConfigPath(), WithEnv: true})
	if err != nil {
		return c, 0, err
	}
	c.ConfigPath = b.Source(ver)
	return c, ver, nil
}

// Source names where the configuration came from, for /metrics config_source.
func (b *Boot) Source(ver int64) string { return fmt.Sprintf("%s#%d", b.DBPath, ver) }

// Fatal prints a start-up error and exits: systemd and Docker restart policies retry.
func Fatal(err error) {
	fmt.Fprintln(os.Stderr, "[Config] ERROR", err)
	os.Exit(1)
}

// PatchSaved merges body onto the saved document, not onto the configuration in use, so
// environment overrides are never written. A key an env var overrides that comes back with
// the env's value (the Control Panel echoes what GET showed) keeps the saved value.
func (b *Boot) PatchSaved(body []byte) (base, next Config, err error) {
	doc, _, err := b.Store.Current()
	if err != nil {
		return base, next, err
	}
	if base, err = Resolve(doc, Env{RootPath: b.Root, LegacyPath: LegacyConfigPath()}); err != nil {
		return base, next, err
	}
	inUse, _ := Resolve(doc, Env{RootPath: b.Root, LegacyPath: LegacyConfigPath(), WithEnv: true})
	if next, err = base.Patch(body); err != nil {
		return base, next, err
	}
	keepEcho := func(n *string, used, saved string) {
		if *n == used && used != saved {
			*n = saved
		}
	}
	keepEcho(&next.GoStormBaseURL, inUse.GoStormBaseURL, base.GoStormBaseURL)
	keepEcho(&next.LogLevel, inUse.LogLevel, base.LogLevel)
	keepEcho(&next.Plex.URL, inUse.Plex.URL, base.Plex.URL)
	keepEcho(&next.Plex.Token, inUse.Plex.Token, base.Plex.Token)
	if next.MasterConcurrencyLimit == inUse.MasterConcurrencyLimit && inUse.MasterConcurrencyLimit != base.MasterConcurrencyLimit {
		next.MasterConcurrencyLimit = base.MasterConcurrencyLimit
	}
	if next.ReadAheadBudgetMB == inUse.ReadAheadBudgetMB && inUse.ReadAheadBudgetMB != base.ReadAheadBudgetMB {
		next.ReadAheadBudgetMB = base.ReadAheadBudgetMB
	}
	return base, next, nil
}

// SaveConfig stores next as a new version and returns the configuration in use.
func (b *Boot) SaveConfig(next Config, source, actor, note string) (Config, int64, bool, error) {
	doc, err := Document(next)
	if err != nil {
		return Config{}, 0, false, err
	}
	_, changed, err := b.Store.Save(doc, source, actor, note)
	if err != nil {
		return Config{}, 0, false, err
	}
	c, ver, err := b.InUse()
	return c, ver, changed, err
}

// ReadSaved is the saved configuration of a DB another process may hold open (tools like
// musicimport): read-only, environment applied, nothing written.
func ReadSaved(dbPath string) (Config, error) {
	db, err := metadb.OpenReadOnly(dbPath)
	if err != nil {
		return Config{}, err
	}
	defer db.Close() // read-only handle: no checkpoint on the engine's DB
	v, ok, err := db.LatestConfigVersion()
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w (has Tiramisu started once with this version?)", dbPath, err)
	}
	if !ok {
		return Config{}, fmt.Errorf("%s: %w", dbPath, ErrNoConfig)
	}
	return Resolve(v.JSON, Env{LegacyPath: LegacyConfigPath(), WithEnv: true})
}

// ErrNoVersion: the version asked for is not in the history (never existed, or pruned).
var ErrNoVersion = errors.New("no such configuration version")

// RestorePreview is what Rollback(id) would change, without saving: the same document
// (telemetry id kept) against the one in use. Secrets compare by value, so A→B→A is no change.
func (b *Boot) RestorePreview(id int64) ([]Change, error) {
	doc, ok, err := b.Store.Version(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoVersion
	}
	cur, _, err := b.Store.Current()
	if err != nil {
		return nil, err
	}
	ch := Diff(cur, b.Store.keepTelemetryID(doc))
	if ch == nil {
		ch = []Change{}
	}
	return ch, nil
}

// Rollback saves version id again as the newest one and returns the configuration in use,
// the new version and what changed against the one it replaces.
func (b *Boot) Rollback(id int64, actor string) (Config, int64, []Change, error) {
	doc, ok, err := b.Store.Version(id)
	if err != nil {
		return Config{}, 0, nil, err
	}
	if !ok {
		return Config{}, 0, nil, ErrNoVersion
	}
	cur, _, err := b.Store.Current()
	if err != nil {
		return Config{}, 0, nil, err
	}
	// A rollback restores settings, not the install's identity.
	doc = b.Store.keepTelemetryID(doc)
	// Resolve before saving: a document that cannot be put in use is not saved.
	if _, err := Resolve(doc, Env{RootPath: b.Root, LegacyPath: LegacyConfigPath(), WithEnv: true}); err != nil {
		return Config{}, 0, nil, err
	}
	if _, _, err := b.Store.Save(doc, "rollback", actor, fmt.Sprintf("rollback to #%d", id)); err != nil {
		return Config{}, 0, nil, err
	}
	c, ver, err := b.InUse()
	ch := Diff(cur, doc)
	if ch == nil {
		ch = []Change{}
	}
	return c, ver, ch, err
}
