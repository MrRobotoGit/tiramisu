package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExportImport(t *testing.T) {
	e := newStoreEnv(t)
	if RunExport(e.s, filepath.Join(e.dir, "x.json")) != 1 {
		t.Fatal("export before any configuration exists must fail clearly")
	}
	e.s.Bootstrap(nil)
	out := filepath.Join(e.dir, "export.json")
	if RunExport(e.s, out) != 0 {
		t.Fatal("export failed")
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0600 {
		t.Fatal("export holds secrets: 0600")
	}
	in := filepath.Join(e.dir, "in.json")
	os.WriteFile(in, []byte(`{"metrics_port": 9191}`), 0600)
	if RunImport(e.s, in) != 0 || e.current(t).MetricsPort != 9191 {
		t.Fatal("import failed")
	}
	if v := e.versions(t); v[0].Source != "cli" {
		t.Fatalf("source: %s", v[0].Source)
	}
	os.WriteFile(in, []byte(`{"metrics_port":`), 0600)
	if RunImport(e.s, in) != 1 || e.current(t).MetricsPort != 9191 {
		t.Fatal("a malformed import must fail and change nothing")
	}
}
