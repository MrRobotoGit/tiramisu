package config

import (
	"os"
	"path/filepath"
	"testing"
)

func newBoot(t *testing.T) *Boot {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TIRAMISU_STATE_DB", "")
	t.Setenv("MKV_PROXY_CONFIG_PATH", filepath.Join(dir, "config.json"))
	b, err := Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(b.StateDir, 0700)
	if err := b.Open(nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.DB.Close() })
	if _, _, err := b.Load(); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPatchSavedNeverStoresEnv(t *testing.T) {
	b := newBoot(t)
	t.Setenv("MKV_PROXY_CONCURRENCY", "7")
	t.Setenv("TIRAMISU_PLEX_URL", "http://env-plex:32400")
	inUse, _, _ := b.InUse()
	if inUse.MasterConcurrencyLimit != 7 || inUse.Plex.URL != "http://env-plex:32400" {
		t.Fatal("env must apply to the configuration in use")
	}
	// The Control Panel posts back what GET showed (env values included) plus one change.
	_, next, err := b.PatchSaved([]byte(`{"master_concurrency_limit": 7, "plex": {"url": "http://env-plex:32400"}, "metrics_port": 9191}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.SaveConfig(next, "ui", "admin", ""); err != nil {
		t.Fatal(err)
	}
	doc, _, _ := b.Store.Current()
	saved, _ := Resolve(doc, Env{})
	if saved.MasterConcurrencyLimit != 25 || saved.Plex.URL != "" || saved.MetricsPort != 9191 {
		t.Fatalf("env leaked into the saved document: limit=%d plex=%q port=%d", saved.MasterConcurrencyLimit, saved.Plex.URL, saved.MetricsPort)
	}
	// A real change to an env-overridden key is kept.
	_, next, _ = b.PatchSaved([]byte(`{"master_concurrency_limit": 30}`))
	b.SaveConfig(next, "ui", "admin", "")
	doc, _, _ = b.Store.Current()
	saved, _ = Resolve(doc, Env{})
	if saved.MasterConcurrencyLimit != 30 {
		t.Fatalf("a real change must be saved: %d", saved.MasterConcurrencyLimit)
	}
}

func TestInUseSource(t *testing.T) {
	b := newBoot(t)
	c, ver, err := b.InUse()
	if err != nil || c.ConfigPath != b.Source(ver) || c.RootPath != b.Root {
		t.Fatalf("source=%q root=%q err=%v", c.ConfigPath, c.RootPath, err)
	}
}

func TestReadSavedNextToRunningEngine(t *testing.T) {
	b := newBoot(t) // the engine keeps its handle open
	_, next, _ := b.PatchSaved([]byte(`{"plex": {"token": "tok"}}`))
	b.SaveConfig(next, "ui", "", "")
	c, err := ReadSaved(b.DBPath)
	if err != nil || c.Plex.Token != "tok" {
		t.Fatalf("ReadSaved: %v %q", err, c.Plex.Token)
	}
	if _, err := ReadSaved(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("a missing DB must not be created")
	}
}
