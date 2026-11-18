package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"tiramisu/internal/metadb"
)

// KeepVersions is how many configuration versions the history keeps.
const KeepVersions = 50

// ErrNoConfig: the DB holds no configuration yet (an install that has not started once).
var ErrNoConfig = errors.New("no configuration saved yet: start Tiramisu once, or use config.json")

// VersionStore is the part of the state DB the configuration needs (*metadb.DB).
type VersionStore interface {
	InsertConfigVersion(metadb.ConfigVersion, int) (int64, error)
	LatestConfigVersion() (metadb.ConfigVersion, bool, error)
	ConfigVersion(int64) (metadb.ConfigVersion, bool, error)
	ListConfigVersions(int) ([]metadb.ConfigVersion, error)
	ConfigMeta(string) (string, bool, error)
	SetConfigMeta(string, string) error
}

// Store saves and loads the configuration as versions in the state DB.
type Store struct {
	db       VersionStore
	stateDir string
	now      func() time.Time
	logf     func(string, ...any)
	rename   func(oldpath, newpath string) error
}

func NewStore(db VersionStore, stateDir string, logf func(string, ...any)) *Store {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Store{db: db, stateDir: stateDir, now: time.Now, logf: logf, rename: os.Rename}
}

// Current is the saved document and its version id.
func (s *Store) Current() ([]byte, int64, error) {
	v, ok, err := s.db.LatestConfigVersion()
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrNoConfig
	}
	return v.JSON, v.ID, nil
}

// Save appends doc as a new version unless it equals the current one (changed=false).
func (s *Store) Save(doc []byte, source, actor, note string) (int64, bool, error) {
	return s.save(doc, source, actor, note, "")
}

func (s *Store) save(doc []byte, source, actor, note, sha string) (int64, bool, error) {
	canon, err := Canonical(doc)
	if err != nil {
		return 0, false, err
	}
	if cur, id, err := s.Current(); err == nil && bytes.Equal(cur, canon) {
		return id, false, nil
	}
	id, err := s.db.InsertConfigVersion(metadb.ConfigVersion{JSON: canon, CreatedAt: s.now().Unix(),
		Source: source, Actor: actor, Note: note, FileSHA: sha}, KeepVersions)
	if err != nil {
		return 0, false, err
	}
	if err := writeAtomic(filepath.Join(s.stateDir, "config.last.json"), canon); err != nil {
		s.logf("[Config] WARN cannot write the cold copy config.last.json: %v", err)
	}
	return id, true, nil
}

// Bootstrap runs at start-up, before the configuration is used: it imports config.json on
// the first start, takes a drop-in on later ones, and fills in what a fresh install lacks.
func (s *Store) Bootstrap(files []string) error {
	latest, has, err := s.db.LatestConfigVersion()
	if err != nil {
		return err
	}
	file, data, sha := "", []byte(nil), ""
	for _, f := range files {
		d, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		h := fileSHA(d)
		if seen, _, _ := s.db.ConfigMeta("seen_sha256:" + f); seen == h {
			continue // left in place on an earlier start: not a drop-in
		}
		if file == "" {
			file, data, sha = f, d, h
			continue
		}
		// Remembered, so it does not become the drop-in once the first one is gone.
		s.logf("[Config] %s ignored: %s is imported first, one file per start; it will not be read unless it changes", f, file)
		s.db.SetConfigMeta("seen_sha256:"+f, h)
	}
	if file != "" {
		if err := s.considerFile(file, data, sha, latest, has); err != nil {
			return err
		}
	}
	if _, _, err := s.Current(); errors.Is(err, ErrNoConfig) {
		doc, _ := Document(defaults())
		if _, _, err := s.Save(doc, "default", "first start", ""); err != nil {
			return err
		}
		s.logf("[Config] no configuration found: starting from the defaults")
	}
	doc, _, err := s.Current()
	if err != nil {
		return err
	}
	if c, err := Resolve(doc, Env{}); err == nil && c.TelemetryID == "" {
		c.TelemetryID = uuid.New().String()
		d, _ := Document(c)
		if _, _, err := s.Save(d, "default", "telemetry id", ""); err != nil {
			return err
		}
	}
	return nil
}

func fileSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Store) considerFile(file string, data []byte, sha string, latest metadb.ConfigVersion, has bool) error {
	lastSHA, imported, _ := s.db.ConfigMeta("last_import_sha256")
	renamed, _, _ := s.db.ConfigMeta("last_import_renamed")
	switch {
	case has && !imported && latest.Source != "default":
		// A configuration nobody imported is in use (--import-config before the first start,
		// or saved since): the file does not replace it.
		s.logf("[Config] %s not imported: a saved configuration is in use (version #%d); use --import-config to replace it", file, latest.ID)
		s.markImported(file, sha, s.retire(file))
		return nil
	case has && imported && sha == lastSHA && renamed == "0":
		return nil // a mounted file that could not be renamed, already imported
	}
	doc, skipped, err := ParseDocumentLenient(data)
	if err != nil {
		if !has {
			return fmt.Errorf("%s is not valid JSON (%v): fix it, nothing was started", file, err)
		}
		s.logf("[Config] ERROR %s is not valid JSON (%v): not imported, the configuration in use is unchanged", file, err)
		return nil
	}
	if skipped != nil {
		s.logf("[Config] WARN %s: %v; that field keeps its default, the rest is imported", file, skipped)
	}
	doc = s.keepTelemetryID(doc)
	id, _, err := s.save(doc, "import", file, "", sha)
	if err != nil {
		return err
	}
	s.logf("[Config] imported %s as version #%d", file, id)
	s.markImported(file, sha, s.retire(file))
	return nil
}

func (s *Store) markImported(file, sha string, renamed bool) {
	s.db.SetConfigMeta("last_import_sha256", sha)
	s.db.SetConfigMeta("last_import_renamed", map[bool]string{true: "1", false: "0"}[renamed])
	if !renamed {
		s.db.SetConfigMeta("seen_sha256:"+file, sha)
	}
}

// keepTelemetryID carries the install's telemetry ID into a document that has none.
func (s *Store) keepTelemetryID(doc []byte) []byte {
	cur, _, err := s.Current()
	if err != nil {
		return doc
	}
	c, err1 := Resolve(doc, Env{})
	old, err2 := Resolve(cur, Env{})
	if err1 != nil || err2 != nil || c.TelemetryID != "" || old.TelemetryID == "" {
		return doc
	}
	c.TelemetryID = old.TelemetryID
	if d, err := Document(c); err == nil {
		return d
	}
	return doc
}

// retire renames an imported file out of the way, owner-only (it holds secrets).
func (s *Store) retire(file string) bool {
	dst := file + ".imported-" + s.now().Format("20060102-150405")
	if err := s.rename(file, dst); err != nil {
		s.logf("[Config] WARN cannot rename %s (%v): it will not be imported again; remove the mount or the file", file, err)
		return false
	}
	os.Chmod(dst, 0600)
	return true
}

// writeAtomic writes data through a temporary file, synced, then renamed: mode 0600.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
