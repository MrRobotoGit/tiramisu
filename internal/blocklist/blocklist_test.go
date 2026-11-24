package blocklist

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newLoop(t *testing.T, installErr error) (*Loop, *atomic.Int32) {
	dir := t.TempDir()
	installs := new(atomic.Int32)
	return &Loop{
		Dir:         func() string { return dir },
		URL:         func() string { return "" },
		CountRanges: func(p string) int { b, _ := os.ReadFile(p); return strings.Count(string(b), "-") },
		Install:     func() error { installs.Add(1); return installErr },
		Logf:        func(string, ...any) {},
		Done:        make(chan struct{}),
		Go:          func(f func()) { go f() },
	}, installs
}

func TestUpdateGzipInstallsAndRecordsURL(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte("bad:1.2.3.4-1.2.3.5\n"))
	w.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(gz.Bytes()) }))
	defer srv.Close()
	l, installs := newLoop(t, nil)
	url := srv.URL + "/list?archiveformat=gz"
	l.Update(url)
	b, err := os.ReadFile(filepath.Join(l.Dir(), "blocklist"))
	if err != nil || !strings.Contains(string(b), "1.2.3.4-1.2.3.5") || installs.Load() != 1 {
		t.Fatalf("file %q err %v installs %d", b, err, installs.Load())
	}
	if u, _ := os.ReadFile(filepath.Join(l.Dir(), "blocklist.url")); string(u) != url {
		t.Fatalf("url sidecar %q", u)
	}
	l.Update(url) // recent and same URL: skipped
	if installs.Load() != 1 {
		t.Fatalf("a recent list was downloaded again")
	}
}

func TestUpdateRefusesAListWithNoRanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("<html>captcha</html>")) }))
	defer srv.Close()
	l, installs := newLoop(t, nil)
	prev := filepath.Join(l.Dir(), "blocklist")
	os.WriteFile(prev, []byte("old:9.9.9.9-9.9.9.9\n"), 0644)
	l.Update(srv.URL)
	if b, _ := os.ReadFile(prev); !strings.Contains(string(b), "old") || installs.Load() != 0 {
		t.Fatalf("previous list replaced: %q installs %d", b, installs.Load())
	}
	if _, err := os.Stat(prev + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left behind")
	}
}

func TestStartStopAndDone(t *testing.T) {
	hits := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		rw.Write([]byte("x:1.1.1.1-1.1.1.2\n"))
	}))
	defer srv.Close()
	l, _ := newLoop(t, nil)
	done := make(chan struct{})
	l.Done = done
	exited := make(chan struct{}, 4)
	l.Go = func(f func()) { go func() { f(); exited <- struct{}{} }() }

	l.Stop() // nothing running: no panic
	l.Start(srv.URL)
	<-hits
	l.Start(srv.URL + "/other") // restart: the first loop exits
	<-exited
	<-hits
	l.Stop()
	<-exited
	l.Stop() // twice: no panic

	l.Start(srv.URL + "/third")
	<-hits
	close(done) // process shutdown ends the loop too
	<-exited
	l.Start("") // empty URL: nothing starts
	select {
	case <-exited:
		t.Fatal("a loop started for an empty URL")
	default:
	}
}
