package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootFromAndPaths(t *testing.T) {
	dir := t.TempDir()
	if RootFrom("") != "/home/pi" || RootFrom(dir) != dir || RootFrom(filepath.Join(dir, "x.db")) != dir {
		t.Fatal("RootFrom must match the old --path resolution")
	}
	t.Setenv("TIRAMISU_STATE_DB", "")
	if DBPath(StateDir(dir)) != filepath.Join(dir, "STATE", "tiramisu.db") {
		t.Fatal(DBPath(StateDir(dir)))
	}
	t.Setenv("TIRAMISU_STATE_DB", "/x/y.db")
	if DBPath("ignored") != "/x/y.db" {
		t.Fatal("TIRAMISU_STATE_DB wins")
	}
}

func TestLegacyFilesDeduplicated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MKV_PROXY_CONFIG_PATH", filepath.Join(dir, "config.json"))
	files := LegacyFiles(dir) // env path and <root>/config.json are the same file
	n := 0
	for _, f := range files {
		if f == filepath.Join(dir, "config.json") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate candidate: %v", files)
	}
}

func TestCheckLegacyDBPath(t *testing.T) {
	dir := t.TempDir()
	state := StateDir(dir)
	f := filepath.Join(dir, "config.json")
	t.Setenv("TIRAMISU_STATE_DB", "")
	os.WriteFile(f, []byte(`{"state_db_path": "/elsewhere/t.db"}`), 0600)
	if CheckLegacyDBPath([]string{f}, state) == nil {
		t.Fatal("a moved DB without TIRAMISU_STATE_DB must be refused")
	}
	os.WriteFile(f, []byte(`{"state_db_path": "`+filepath.Join(state, "tiramisu.db")+`"}`), 0600)
	if err := CheckLegacyDBPath([]string{f}, state); err != nil {
		t.Fatalf("the default spelled out is fine: %v", err)
	}
	// Audit M1 #4: relative state dir (--path .), absolute default in the file.
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	os.WriteFile(f, []byte(`{"state_db_path": "`+filepath.Join(dir, "STATE", "tiramisu.db")+`"}`), 0600)
	if err := CheckLegacyDBPath([]string{f}, "STATE"); err != nil {
		t.Fatalf("absolute default against a relative state dir: %v", err)
	}
	os.WriteFile(f, []byte(`{"state_db_path": "/elsewhere/t.db"}`), 0600)
	t.Setenv("TIRAMISU_STATE_DB", "/elsewhere/t.db")
	if err := CheckLegacyDBPath([]string{f}, state); err != nil {
		t.Fatalf("with TIRAMISU_STATE_DB set it is fine: %v", err)
	}
}
