package library

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var stubObserver atomic.Pointer[func(path string)]

// SetStubObserver is told the path of every stub written or removed through this package (nil: none).
func SetStubObserver(f func(path string)) {
	if f == nil {
		stubObserver.Store(nil)
		return
	}
	stubObserver.Store(&f)
}

func notifyStub(path string) {
	if f := stubObserver.Load(); f != nil {
		(*f)(path)
	}
}

// RemoveStub deletes a stub and tells the observer; a missing file is not an error.
func RemoveStub(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	notifyStub(path)
	return nil
}

// DeleteStub is RemoveStub for a caller that must report a missing file (the FUSE unlink).
func DeleteStub(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	notifyStub(path)
	return nil
}

// StubInfo is what a slave needs to write the same stub: Path is relative to the media root.
type StubInfo struct {
	Path     string
	Hash     string
	Index    int
	Size     int64
	IMDB     string // movie stubs only
	Key      string // TV registry key
	Score    int
	ShowIMDB string
}

// StubEntry reads the stub at root/rel; false when there is none.
func StubEntry(root, rel string, reg EpisodeRegistry) (StubInfo, bool, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err != nil {
		if os.IsNotExist(err) {
			return StubInfo{}, false, nil
		}
		return StubInfo{}, false, err
	}
	st := readStub(abs)
	out := StubInfo{Path: filepath.ToSlash(rel), Hash: st.Hash, Index: st.FileIndex, Size: st.Size}
	if strings.HasPrefix(out.Path, "movies/") {
		out.IMDB = st.IMDB
	}
	if strings.HasPrefix(out.Path, "tv/") && reg != nil {
		eps, err := reg.EpisodesByFilePath(abs)
		if err != nil {
			return StubInfo{}, false, err
		}
		if len(eps) > 0 {
			out.Key, out.Score, out.ShowIMDB = eps[0].EpisodeKey, eps[0].QualityScore, eps[0].ShowIMDB
		}
	}
	return out, true, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// processStart separates temp files a crash left (older) from those of live writers.
var processStart = time.Now()

// RemoveStaleStubTemps deletes the stub temp files under dirs that predate this process: a crash
// between write and rename left them. It reports how many went.
func RemoveStaleStubTemps(dirs ...string) int {
	n := 0
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() && p != dir {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !isStubTemp(d.Name()) {
				return nil
			}
			if fi, err := d.Info(); err == nil && fi.ModTime().Before(processStart) && os.Remove(p) == nil {
				n++
			}
			return nil
		})
	}
	return n
}

// isStubTemp matches WriteStubData's temp names, and the single one of older builds.
func isStubTemp(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".tmp") && strings.Contains(l, ".mkv.")
}
