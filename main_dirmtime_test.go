package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"tiramisu/internal/library"
)

// Spec 7: an audio directory's mtime moves when its committed child set does, and at
// no other time. Staging noise, reads and restarts must leave it alone.
func TestAudioDirectoryMtimeIsDeterministic(t *testing.T) {
	const rel = "Artist/Album/01 - Track_01234567.flac"
	first := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	second := first.Add(2 * time.Hour)

	row := committedA(rel)
	row.UpdatedAtNS = first.UnixNano()

	e := newVFSEnv(t)
	phys := e.phys(library.SectionMusic, rel)
	writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
	e.publish(row)

	dirMtime := func(rel string) time.Time {
		t.Helper()
		res := e.lookup("music/" + rel)
		if res.status != fuse.OK {
			t.Fatalf("lookup music/%s status = %v, want OK", rel, res.status)
		}
		attr := e.getattr(t, res.nodeID)
		return time.Unix(int64(attr.Mtime), int64(attr.Mtimensec))
	}

	if got := dirMtime("Artist"); !got.Equal(first) {
		t.Errorf("album chain mtime = %v, want the committed update time %v", got, first)
	}
	if got := dirMtime("Artist/Album"); !got.Equal(first) {
		t.Errorf("album mtime = %v, want %v", got, first)
	}

	// A restart rebuilds the node tree; the derived mtime is unchanged because it
	// comes from the registry, not from the filesystem.
	e.remount()
	if got := dirMtime("Artist"); !got.Equal(first) {
		t.Errorf("mtime after remount = %v, want %v", got, first)
	}

	// Staging that never commits leaves no trace: create and remove a staging name
	// with a fresh physical timestamp.
	staging := filepath.Join(e.root, string(library.SectionMusic), "Artist", "Album", ".tiramisu-txn-0-0")
	writeFile(t, staging, "staged", second)
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	if got := dirMtime("Artist/Album"); !got.Equal(first) {
		t.Errorf("mtime after rolled-back staging = %v, want %v", got, first)
	}

	// A committed add moves it, and the other section stays where it was.
	add := committedA("Artist/Album/02 - Track_01234567.flac")
	add.UpdatedAtNS = second.UnixNano()
	add.FileIndex = idxA + 1
	writeFile(t, e.phys(library.SectionMusic, add.VirtualPath), jsonStub(streamURL(hashA, add.FileIndex), rowSize), rowMtime)
	e.publish(row, add)
	if got := dirMtime("Artist/Album"); !got.Equal(second) {
		t.Errorf("mtime after a committed add = %v, want %v", got, second)
	}
}
