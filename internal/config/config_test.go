package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The blocklist filter must arrive as a working default: a config written before the
// setting existed has to get it, while an explicit empty string still means "no filter".
func TestBlockListFilterDefault(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) Config {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MKV_PROXY_CONFIG_PATH", p)
		return LoadConfig()
	}

	if got := load(`{"metrics_port": 9080}`).BlockListFilter; got != DefaultBlockListFilter {
		t.Errorf("key absent: filter = %q, want the default %q", got, DefaultBlockListFilter)
	}
	if got := load(`{"blocklist_filter": ""}`).BlockListFilter; got != "" {
		t.Errorf("explicit empty: filter = %q, want it left empty", got)
	}
	if got := load(`{"blocklist_filter": "custom"}`).BlockListFilter; got != "custom" {
		t.Errorf("explicit value: filter = %q, want %q", got, "custom")
	}
}

// The blocklist URL is a working default too: an empty field in the Control Panel is
// skipped on save, so a configuration without the key could never acquire one.
func TestBlockListURLDefault(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) Config {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MKV_PROXY_CONFIG_PATH", p)
		return LoadConfig()
	}

	if got := load(`{"metrics_port": 9080}`).BlockListURL; got != DefaultBlockListURL {
		t.Errorf("key absent: url = %q, want the default", got)
	}
	if got := load(`{"blocklist_url": "https://example.invalid/list.gz"}`).BlockListURL; got != "https://example.invalid/list.gz" {
		t.Errorf("explicit value not kept: %q", got)
	}
}

// A limit of 0 makes masterDataSemaphore unbuffered, and the non-blocking send that claims a
// pump slot can then never succeed: nothing streams.
func TestFinalizeFloorsMasterConcurrencyLimit(t *testing.T) {
	for _, in := range []int{0, -3} {
		c := &Config{MasterConcurrencyLimit: in}
		c.finalize()
		if c.MasterConcurrencyLimit < 1 {
			t.Errorf("MasterConcurrencyLimit %d stayed %d, want >= 1", in, c.MasterConcurrencyLimit)
		}
		if c.ConcurrencyLimit < 1 || c.MaxConcurrentHTTP < 1 || c.MaxConnsPerHost < 1 {
			t.Errorf("derived limits not floored: conc=%d http=%d conns=%d",
				c.ConcurrencyLimit, c.MaxConcurrentHTTP, c.MaxConnsPerHost)
		}
	}
}

func TestFinalizeLeavesValidMasterConcurrencyLimit(t *testing.T) {
	c := &Config{MasterConcurrencyLimit: 25}
	c.finalize()
	if c.MasterConcurrencyLimit != 25 {
		t.Errorf("MasterConcurrencyLimit = %d, want 25", c.MasterConcurrencyLimit)
	}
}

// The knob is config.json-only, so it must ship off: nobody can turn it back off from
// the Control Panel. An explicit value has to survive, since that file is the only way in.
func TestFuseMaxInflightDefault(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) Config {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MKV_PROXY_CONFIG_PATH", p)
		return LoadConfig()
	}

	if got := load(`{"metrics_port": 9080}`).FuseMaxInflightMB; got != 0 {
		t.Errorf("key absent: FuseMaxInflightMB = %d, want the cap off by default", got)
	}
	if got := load(`{"fuse_max_inflight_mb": 256}`).FuseMaxInflightMB; got != 256 {
		t.Errorf("explicit 256: FuseMaxInflightMB = %d, want it honoured", got)
	}
	if got := load(`{"fuse_max_inflight_mb": 512}`).FuseMaxInflightMB; got != 512 {
		t.Errorf("explicit value: FuseMaxInflightMB = %d, want 512", got)
	}
}

// 0 must survive finalize: it is what leaves go-fuse unbounded, and a floor
// applied to it would silently enable backpressure nobody asked for.
func TestFinalizeFuseMaxInflight(t *testing.T) {
	for _, tc := range []struct{ in, want int64 }{
		{0, 0},   // unlimited, the default
		{-5, 0},  // nonsense normalises to unlimited, not to the floor
		{1, 16},  // below one batch of requests it would serialise FUSE
		{16, 16}, // the floor itself is untouched
		{256, 256},
	} {
		c := &Config{MasterConcurrencyLimit: 25, FuseMaxInflightMB: tc.in}
		c.finalize()
		if c.FuseMaxInflightMB != tc.want {
			t.Errorf("FuseMaxInflightMB %d finalised to %d, want %d", tc.in, c.FuseMaxInflightMB, tc.want)
		}
	}
}
