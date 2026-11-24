package autoremove

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const blHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// A blacklist that cannot be parsed is set aside, never rewritten in place: its bytes
// stay for whoever repairs it, and new removals for good are recorded again.
func TestAddToBlacklistSetsAsideAnUnreadableFile(t *testing.T) {
	tr := &Remover{logger: log.New(io.Discard, "", 0)}
	dir := t.TempDir()
	path := filepath.Join(dir, "blacklist.json")
	corrupt := []byte(`{"hashes":{"bbbb": "Old Film"`)
	if err := os.WriteFile(path, corrupt, 0644); err != nil {
		t.Fatal(err)
	}
	if err := tr.addToBlacklistAt(path, blHash, "New Film"); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(path + ".corrupt-*")
	if len(aside) != 1 {
		t.Fatalf("set aside = %v", aside)
	}
	if got, _ := os.ReadFile(aside[0]); string(got) != string(corrupt) {
		t.Fatalf("the corrupt bytes were not kept: %s", got)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), blHash) {
		t.Fatalf("new blacklist = %s", got)
	}
}

// Two removals at once must both land: an unlocked read-modify-write loses one.
func TestAddToBlacklistConcurrentWritersKeepEveryHash(t *testing.T) {
	tr := &Remover{logger: log.New(io.Discard, "", 0)}
	path := filepath.Join(t.TempDir(), "blacklist.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := fmt.Sprintf("%040x", i+1)
			if err := tr.addToBlacklistAt(path, h, fmt.Sprintf("Film %d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got, _ := os.ReadFile(path)
	for i := 0; i < 20; i++ {
		if h := fmt.Sprintf("%040x", i+1); !strings.Contains(string(got), h) {
			t.Fatalf("hash %s lost", h)
		}
	}
}

func TestAddToBlacklistAppends(t *testing.T) {
	tr := &Remover{logger: log.New(io.Discard, "", 0)}
	path := filepath.Join(t.TempDir(), "blacklist.json")
	if err := tr.addToBlacklistAt(path, strings.ToUpper(blHash), "New Film"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hashes":null,"titles":null}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := tr.addToBlacklistAt(path, blHash, "New Film"); err != nil {
		t.Fatalf("null fields: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), blHash) || !strings.Contains(string(got), "newfilm") {
		t.Fatalf("blacklist = %s", got)
	}
}
