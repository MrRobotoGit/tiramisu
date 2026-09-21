package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"tiramisu/internal/library"
)

// Spec 4: audio projections are read-only to their clients. The API owns every
// mutation, so the filesystem answers EROFS (and EPERM for a direct unlink) instead
// of pretending the caller may change bytes the engine serves.
func TestAudioReadOnlySemantics(t *testing.T) {
	const rel = "Artist/Album/01 - Track_01234567.flac"
	const albumRel = "Artist/Album"

	t.Run("file mode is 0444 and audio directories are 0555", func(t *testing.T) {
		e := newVFSEnv(t)
		phys := e.phys(library.SectionMusic, rel)
		e.publish(committedA(rel))
		writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)

		file := e.lookup("music/" + rel)
		if file.status != fuse.OK {
			t.Fatalf("lookup audio file status = %v, want OK", file.status)
		}
		if got := e.getattr(t, file.nodeID).Mode & 0o777; got != 0o444 {
			t.Errorf("audio file mode = %04o, want 0444", got)
		}

		album := e.lookup("music/" + albumRel)
		if album.status != fuse.OK {
			t.Fatalf("lookup audio directory status = %v, want OK", album.status)
		}
		if got := e.getattr(t, album.nodeID).Mode & 0o777; got != 0o555 {
			t.Errorf("audio directory mode = %04o, want 0555", got)
		}

		// Video keeps the historical writable-looking attributes.
		movies := filepath.Join(e.root, string(library.SectionMovies))
		if err := os.MkdirAll(movies, 0o755); err != nil {
			t.Fatal(err)
		}
		video := e.lookup("movies")
		if video.status != fuse.OK {
			t.Fatalf("lookup movies status = %v, want OK", video.status)
		}
		if got := e.getattr(t, video.nodeID).Mode & 0o777; got != 0o755 {
			t.Errorf("video directory mode = %04o, want 0755", got)
		}
	})

	t.Run("writable open and every metadata mutation are EROFS", func(t *testing.T) {
		e := newVFSEnv(t)
		phys := e.phys(library.SectionMusic, rel)
		e.publish(committedA(rel))
		writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)

		file := e.lookup("music/" + rel)
		album := e.lookup("music/" + albumRel)
		if file.status != fuse.OK || album.status != fuse.OK {
			t.Fatalf("lookups = (%v, %v), want OK", file.status, album.status)
		}

		var openOut fuse.OpenOut
		for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_RDONLY | syscall.O_TRUNC} {
			if st := e.rfs.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: file.nodeID}, Flags: flags}, &openOut); st != fuse.Status(syscall.EROFS) {
				t.Errorf("Open(flags %#x) = %v, want EROFS", flags, st)
			}
		}

		var setIn fuse.SetAttrIn
		setIn.NodeId = file.nodeID
		setIn.Valid = fuse.FATTR_MODE
		if st := e.rfs.SetAttr(nil, &setIn, &fuse.AttrOut{}); st != fuse.Status(syscall.EROFS) {
			t.Errorf("SetAttr(file) = %v, want EROFS", st)
		}
		var dirSetIn fuse.SetAttrIn
		dirSetIn.NodeId = album.nodeID
		dirSetIn.Valid = fuse.FATTR_MODE
		if st := e.rfs.SetAttr(nil, &dirSetIn, &fuse.AttrOut{}); st != fuse.Status(syscall.EROFS) {
			t.Errorf("SetAttr(dir) = %v, want EROFS", st)
		}

		var mkdirIn fuse.MkdirIn
		mkdirIn.NodeId = album.nodeID
		if st := e.rfs.Mkdir(nil, &mkdirIn, "new", &fuse.EntryOut{}); st != fuse.Status(syscall.EROFS) {
			t.Errorf("Mkdir = %v, want EROFS", st)
		}
		var createIn fuse.CreateIn
		createIn.NodeId = album.nodeID
		if st := e.rfs.Create(nil, &createIn, "new.flac", &fuse.CreateOut{}); st != fuse.Status(syscall.EROFS) {
			t.Errorf("Create = %v, want EROFS", st)
		}
		var renameIn fuse.RenameIn
		renameIn.NodeId = album.nodeID
		renameIn.Newdir = album.nodeID
		if st := e.rfs.Rename(nil, &renameIn, "01 - Track_01234567.flac", "02 - Track_01234567.flac"); st != fuse.Status(syscall.EROFS) {
			t.Errorf("Rename = %v, want EROFS", st)
		}

		// Lifecycle stays API-owned: a direct unlink is refused, not performed.
		if st := e.rfs.Unlink(nil, &fuse.InHeader{NodeId: album.nodeID}, "01 - Track_01234567.flac"); st != fuse.Status(syscall.EPERM) {
			t.Errorf("Unlink = %v, want EPERM", st)
		}
		if _, err := os.Stat(phys); err != nil {
			t.Errorf("unlink attempt touched the file: %v", err)
		}
	})

	t.Run("video mutations keep the historical ENOSYS", func(t *testing.T) {
		e := newVFSEnv(t)
		movies := filepath.Join(e.root, string(library.SectionMovies))
		if err := os.MkdirAll(filepath.Join(movies, "Film"), 0o755); err != nil {
			t.Fatal(err)
		}
		film := e.lookup("movies/Film")
		if film.status != fuse.OK {
			t.Fatalf("lookup movies/Film status = %v, want OK", film.status)
		}

		var mkdirIn fuse.MkdirIn
		mkdirIn.NodeId = film.nodeID
		if st := e.rfs.Mkdir(nil, &mkdirIn, "new", &fuse.EntryOut{}); st != fuse.Status(syscall.ENOSYS) {
			t.Errorf("video Mkdir = %v, want ENOSYS", st)
		}
		var dirSetIn fuse.SetAttrIn
		dirSetIn.NodeId = film.nodeID
		dirSetIn.Valid = fuse.FATTR_MODE
		if st := e.rfs.SetAttr(nil, &dirSetIn, &fuse.AttrOut{}); st != fuse.Status(syscall.ENOSYS) {
			t.Errorf("video SetAttr = %v, want ENOSYS", st)
		}
	})
}
