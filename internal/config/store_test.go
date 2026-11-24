package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tiramisu/internal/metadb"
)

type storeEnv struct {
	db    *metadb.DB
	s     *Store
	dir   string
	file  string
	state string
	logs  []string
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "STATE")
	os.MkdirAll(state, 0700)
	db, err := metadb.New(filepath.Join(state, "tiramisu.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &storeEnv{db: db, dir: dir, file: filepath.Join(dir, "config.json"), state: state}
	e.s = NewStore(db, state, func(f string, a ...any) { e.logs = append(e.logs, f) })
	return e
}

func (e *storeEnv) versions(t *testing.T) []metadb.ConfigVersion {
	t.Helper()
	v, err := e.db.ListConfigVersions(100)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *storeEnv) current(t *testing.T) Config {
	t.Helper()
	doc, _, err := e.s.Current()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Resolve(doc, Env{})
	return c
}

func TestBootstrapFirstStartImports(t *testing.T) {
	e := newStoreEnv(t)
	os.WriteFile(e.file, []byte(`{"metrics_port": 9091, "plex": {"token": "secret"}}`), 0644)
	if err := e.s.Bootstrap([]string{e.file}); err != nil {
		t.Fatal(err)
	}
	if c := e.current(t); c.MetricsPort != 9091 || c.Plex.Token != "secret" || c.TelemetryID == "" {
		t.Fatalf("import: %+v", c.MetricsPort)
	}
	if _, err := os.Stat(e.file); err == nil {
		t.Fatal("imported file must be renamed")
	}
	m, _ := filepath.Glob(e.file + ".imported-*")
	if len(m) != 1 {
		t.Fatalf("renamed copy: %v", m)
	}
	if fi, _ := os.Stat(m[0]); fi.Mode().Perm() != 0600 {
		t.Fatal("renamed copy keeps secrets: must be 0600")
	}
	if fi, err := os.Stat(filepath.Join(e.state, "config.last.json")); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("cold copy: %v", err)
	}
	n := len(e.versions(t))
	if err := e.s.Bootstrap([]string{e.file}); err != nil || len(e.versions(t)) != n {
		t.Fatal("second start without a file must add nothing")
	}
}

func TestBootstrapMalformedFirstStartRefuses(t *testing.T) {
	e := newStoreEnv(t)
	os.WriteFile(e.file, []byte(`{"metrics_port": 9091,`), 0644)
	if err := e.s.Bootstrap([]string{e.file}); err == nil {
		t.Fatal("a broken config.json on the first start must stop, not run on defaults")
	}
	if _, err := os.Stat(e.file); err != nil {
		t.Fatal("the broken file stays where it is")
	}
	if len(e.versions(t)) != 0 {
		t.Fatal("nothing saved")
	}
}

func TestBootstrapDefaultsThenDropIn(t *testing.T) {
	e := newStoreEnv(t)
	if err := e.s.Bootstrap([]string{e.file}); err != nil {
		t.Fatal(err)
	}
	if v := e.versions(t); len(v) == 0 || v[len(v)-1].Source != "default" {
		t.Fatalf("fresh install: %+v", v)
	}
	os.WriteFile(e.file, []byte(`{"metrics_port": 9092}`), 0644)
	e.s.Bootstrap([]string{e.file})
	if e.current(t).MetricsPort != 9092 {
		t.Fatal("a config.json dropped in on a defaults-only install must be imported")
	}
	os.WriteFile(e.file, []byte(`{"metrics_port":`), 0644)
	if err := e.s.Bootstrap([]string{e.file}); err != nil {
		t.Fatalf("a malformed drop-in later on is logged, not fatal: %v", err)
	}
	if e.current(t).MetricsPort != 9092 {
		t.Fatal("malformed drop-in must leave the configuration as it was")
	}
}

func TestBootstrapMountedFileNotReimported(t *testing.T) {
	e := newStoreEnv(t)
	e.s.rename = func(string, string) error { return errors.New("EBUSY") } // a Docker single-file mount
	os.WriteFile(e.file, []byte(`{"metrics_port": 9093}`), 0644)
	e.s.Bootstrap([]string{e.file})
	for i := 0; i < 60; i++ { // well past the 50 kept versions
		c := e.current(t)
		c.ProxyListenPort = 9000 + i
		d, _ := Document(c)
		e.s.Save(d, "ui", "admin", "")
	}
	e.s.Bootstrap([]string{e.file})
	if c := e.current(t); c.ProxyListenPort != 9059 {
		t.Fatalf("the mounted file was re-imported over the UI changes: port %d", c.ProxyListenPort)
	}
}

func TestBootstrapRestoredCopyIsImported(t *testing.T) {
	e := newStoreEnv(t)
	body := []byte(`{"metrics_port": 9094}`)
	os.WriteFile(e.file, body, 0644)
	e.s.Bootstrap([]string{e.file})
	c := e.current(t)
	c.MetricsPort = 9999
	d, _ := Document(c)
	e.s.Save(d, "ui", "admin", "")
	os.WriteFile(e.file, body, 0644) // the user copies config.json.imported-* back to undo
	e.s.Bootstrap([]string{e.file})
	if e.current(t).MetricsPort != 9094 {
		t.Fatal("a file put back by hand must be imported even with the same content as the last import")
	}
}

func TestBootstrapCLIImportWinsOverFile(t *testing.T) {
	e := newStoreEnv(t)
	doc, _ := Canonical([]byte(`{"metrics_port": 9095}`))
	e.s.Save(doc, "cli", "/tmp/x.json", "")
	os.WriteFile(e.file, []byte(`{"metrics_port": 9096}`), 0644)
	e.s.Bootstrap([]string{e.file})
	if e.current(t).MetricsPort != 9095 {
		t.Fatal("--import-config before the first start must not be overwritten by config.json")
	}
	if _, err := os.Stat(e.file); err == nil {
		t.Fatal("the file is moved out of the way")
	}
}

func TestBootstrapOneFilePerStart(t *testing.T) {
	e := newStoreEnv(t)
	second := filepath.Join(e.dir, "other.json")
	os.WriteFile(e.file, []byte(`{"metrics_port": 9097}`), 0644)
	os.WriteFile(second, []byte(`{"metrics_port": 9098}`), 0644)
	e.s.Bootstrap([]string{e.file, second})
	if e.current(t).MetricsPort != 9097 {
		t.Fatal("first candidate wins")
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("the other file is left alone")
	}
	found := false
	for _, l := range e.logs {
		if strings.Contains(l, "ignored") {
			found = true
		}
	}
	if !found {
		t.Fatal("the ignored file must be logged")
	}
}

func TestSaveUnchangedAddsNothing(t *testing.T) {
	e := newStoreEnv(t)
	e.s.Bootstrap(nil)
	doc, id, _ := e.s.Current()
	if nid, changed, err := e.s.Save(doc, "ui", "admin", ""); err != nil || changed || nid != id {
		t.Fatalf("same document: id %d→%d changed=%v err=%v", id, nid, changed, err)
	}
}

// Audit M1 #1: a wrongly typed field must not stop the start; the rest is imported.
func TestBootstrapWrongTypeImportsTheRest(t *testing.T) {
	e := newStoreEnv(t)
	os.WriteFile(e.file, []byte(`{"metrics_port":"9091","fuse_mount_path":"/mnt/x"}`), 0644)
	if err := e.s.Bootstrap([]string{e.file}); err != nil {
		t.Fatalf("type error must not be fatal: %v", err)
	}
	if c := e.current(t); c.FuseMountPath != "/mnt/x" || c.MetricsPort != 9080 {
		t.Fatalf("rest of the file: mount=%q port=%d", c.FuseMountPath, c.MetricsPort)
	}
}

// Audit M1 #2: a second file only logged must not become the drop-in on the next start.
func TestBootstrapIgnoredFileStaysIgnored(t *testing.T) {
	e := newStoreEnv(t)
	second := filepath.Join(e.dir, "other.json")
	os.WriteFile(e.file, []byte(`{"metrics_port": 1111}`), 0644)
	os.WriteFile(second, []byte(`{"metrics_port": 3333}`), 0644)
	e.s.Bootstrap([]string{e.file, second})
	c := e.current(t)
	c.LogLevel = "DEBUG"
	d, _ := Document(c)
	e.s.Save(d, "ui", "admin", "")
	e.s.Bootstrap([]string{e.file, second})
	if c := e.current(t); c.MetricsPort != 1111 || c.LogLevel != "DEBUG" {
		t.Fatalf("stale second file imported: port=%d level=%s", c.MetricsPort, c.LogLevel)
	}
	os.WriteFile(second, []byte(`{"metrics_port": 4444}`), 0644) // edited on purpose: a real drop-in
	e.s.Bootstrap([]string{e.file, second})
	if e.current(t).MetricsPort != 4444 {
		t.Fatal("a changed file is a drop-in again")
	}
}

// Audit M1 #3: CLI import first, then a mounted file that cannot be renamed.
func TestBootstrapCLIThenMountedFile(t *testing.T) {
	e := newStoreEnv(t)
	e.s.rename = func(string, string) error { return errors.New("EBUSY") }
	doc, _ := Canonical([]byte(`{"metrics_port": 2222}`))
	e.s.Save(doc, "cli", "/tmp/x.json", "")
	os.WriteFile(e.file, []byte(`{"metrics_port": 1111}`), 0644)
	e.s.Bootstrap([]string{e.file}) // adds the telemetry version
	e.s.Bootstrap([]string{e.file})
	if e.current(t).MetricsPort != 2222 {
		t.Fatal("the mounted file overwrote the CLI import")
	}
}

// Audit M1 #12: a drop-in without telemetry_id keeps the install's identity.
func TestDropInKeepsTelemetryID(t *testing.T) {
	e := newStoreEnv(t)
	e.s.Bootstrap(nil)
	id := e.current(t).TelemetryID
	os.WriteFile(e.file, []byte(`{"metrics_port": 9099}`), 0644)
	e.s.Bootstrap([]string{e.file})
	if c := e.current(t); c.MetricsPort != 9099 || c.TelemetryID != id {
		t.Fatalf("telemetry id changed: %s -> %s", id, c.TelemetryID)
	}
}
