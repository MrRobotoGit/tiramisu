package vfs

import (
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// A cache-miss Readdir walks the directory, then stores the listing. An invalidation
// that lands during the walk must not be overwritten by that stale result: it would
// survive for the whole TTL and a scan would keep reading the old listing.
func TestDirCachePutIfGenerationRejectsStaleWrites(t *testing.T) {
	dc := NewDirCache(time.Minute)
	generation := dc.Generation("album")

	// The listing was read, then an add invalidated the directory before it landed.
	dc.Delete("album")
	if dc.PutIfGeneration("album", []fuse.DirEntry{{Name: "stale"}}, generation) {
		t.Fatal("PutIfGeneration stored a listing that predates the invalidation")
	}
	if _, ok := dc.Get("album"); ok {
		t.Fatal("stale listing is visible after the invalidation")
	}

	// With no invalidation in between, the same call stores the listing.
	generation = dc.Generation("album")
	if !dc.PutIfGeneration("album", []fuse.DirEntry{{Name: "fresh"}}, generation) {
		t.Fatal("PutIfGeneration refused a listing with no invalidation in between")
	}
	entries, ok := dc.Get("album")
	if !ok || len(entries) != 1 || entries[0].Name != "fresh" {
		t.Fatalf("Get after a fresh Put = (%v, %v), want the fresh listing", entries, ok)
	}
}
