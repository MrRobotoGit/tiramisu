package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestTrialFirstStart runs the first start of an upgrade on a copy of a real install, by
// hand: TIRAMISU_TRIAL_ROOT=<dir with STATE/ and config.json> go test -run TestTrialFirstStart
func TestTrialFirstStart(t *testing.T) {
	root := os.Getenv("TIRAMISU_TRIAL_ROOT")
	if root == "" {
		t.Skip("manual trial")
	}
	file := filepath.Join(root, "config.json")
	t.Setenv("MKV_PROXY_CONFIG_PATH", file)
	want := LoadConfig() // what the old engine would run with
	b, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Open(nil, t.Logf); err != nil {
		t.Fatal(err)
	}
	defer b.DB.Close()
	got, ver, err := b.Load()
	if err != nil {
		t.Fatal(err)
	}
	want.TelemetryID, got.TelemetryID = "", ""
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if string(wj) != string(gj) {
		t.Fatalf("configuration in use differs from the old loader:\nold %s\nnew %s", wj, gj)
	}
	if want.LogDir != got.LogDir || want.RootPath != "" && want.RootPath != got.RootPath {
		t.Fatalf("derived: logdir %q/%q", want.LogDir, got.LogDir)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("config.json not renamed")
	}
	t.Logf("first start OK: version #%d, source %s, logdir %s", ver, got.ConfigPath, got.LogDir)
	got2, ver2, err := b.Load()
	if err != nil || ver2 != ver || got2.MetricsPort != got.MetricsPort {
		t.Fatalf("second start: ver %d→%d err %v", ver, ver2, err)
	}
}
