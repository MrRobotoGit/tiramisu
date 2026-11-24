// Package blocklist downloads the BitTorrent IP blocklist and keeps it fresh.
package blocklist

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Loop refreshes the blocklist file in Dir every 24 hours and hands each new list to Install.
type Loop struct {
	Dir         func() string // where the file goes; "" falls back to the executable's directory
	URL         func() string // the URL in use at each refresh
	CountRanges func(path string) int
	Install     func() error // loads the file just written into the running engine
	Logf        func(format string, args ...any)
	Done        <-chan struct{} // process shutdown
	Go          func(func())    // starts the refresh goroutine (panic-safe in production)

	mu   sync.Mutex
	stop chan struct{}
}

// Start (re)starts the refresh loop for urlStr, stopping a running one first.
func (l *Loop) Start(urlStr string) {
	if urlStr == "" {
		return
	}
	l.mu.Lock()
	if l.stop != nil {
		close(l.stop)
	}
	stop := make(chan struct{})
	l.stop = stop
	l.mu.Unlock()

	l.Go(func() {
		l.Update(urlStr)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				l.Update(l.URL())
			case <-stop:
				return
			case <-l.Done:
				return
			}
		}
	})
}

// Stop signals the running refresh loop, if any, to exit.
func (l *Loop) Stop() {
	l.mu.Lock()
	if l.stop != nil {
		close(l.stop)
		l.stop = nil
	}
	l.mu.Unlock()
}

// Update downloads urlStr into Dir/blocklist unless the file there is recent and from the
// same URL, then installs it.
func (l *Loop) Update(urlStr string) {
	if urlStr == "" {
		return
	}

	// Write next to the rest of the state, not next to the binary: in a container the
	// binary lives in the image layer, so the list was re-downloaded on every start.
	destDir := l.Dir()
	if destDir == "" {
		exePath, err := os.Executable()
		if err != nil {
			l.Logf("[BlockList] Error getting executable path: %v", err)
			return
		}
		destDir = filepath.Dir(exePath)
	}
	destPath := filepath.Join(destDir, "blocklist")

	// A sidecar records which URL produced the file on disk. Age alone is not enough:
	// changing the URL in the Control Panel used to keep serving the previous list for
	// up to 24h, restart included, while the log claimed everything was fine.
	urlPath := destPath + ".url"
	prevURL := ""
	if b, err := os.ReadFile(urlPath); err == nil {
		prevURL = strings.TrimSpace(string(b))
	}
	if info, err := os.Stat(destPath); err == nil && prevURL == urlStr {
		if time.Since(info.ModTime()) < 24*time.Hour {
			l.Logf("[BlockList] Existing blocklist is recent, skipping update")
			return
		}
	}
	if prevURL != "" && prevURL != urlStr {
		l.Logf("[BlockList] URL changed, refreshing now")
	}

	l.Logf("[BlockList] Updating from %s...", urlStr)

	// A bare http.Get has no deadline: a stalled connection (e.g. a VPN flap mid-download)
	// hangs this goroutine forever. Since this call runs before the loop's select, closing
	// the stop channel can't interrupt it either — a timeout is the only way out.
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(urlStr)
	if err != nil {
		l.Logf("[BlockList] Download error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		l.Logf("[BlockList] Download failed: status %d", resp.StatusCode)
		return
	}

	// Detect gzip from the magic bytes, not the URL: iblocklist and friends serve a
	// gzip stream from a query string (...&archiveformat=gz), and a suffix test would
	// save the compressed bytes verbatim - a blocklist with zero usable ranges and no
	// error anywhere.
	buffered := bufio.NewReader(resp.Body)
	var reader io.Reader = buffered
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			l.Logf("[BlockList] Gzip error: %v", err)
			return
		}
		defer gz.Close()
		reader = gz
	}

	// Write to a temp file and rename atomically, so a download that dies mid-copy
	// (network drop, process kill) can't leave destPath truncated and destroy the
	// last known-good blocklist.
	tmpPath := destPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		l.Logf("[BlockList] File create error: %v", err)
		return
	}

	n, err := io.Copy(out, reader)
	if err != nil {
		out.Close()
		os.Remove(tmpPath)
		l.Logf("[BlockList] File write error: %v", err)
		return
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		l.Logf("[BlockList] File close error: %v", err)
		return
	}
	// Refuse to install something with no usable ranges. iblocklist serves a captcha page
	// to anything that looks like a browser, and a downloaded HTML page would otherwise
	// replace a working blocklist with an empty one, silently.
	if ranges := l.CountRanges(tmpPath); ranges == 0 {
		os.Remove(tmpPath)
		l.Logf("[BlockList] Refused: %d bytes downloaded but no usable ranges — keeping the previous list", n)
		return
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		l.Logf("[BlockList] File rename error: %v", err)
		return
	}
	if err := os.WriteFile(urlPath, []byte(urlStr), 0644); err != nil {
		l.Logf("[BlockList] WARNING: could not record the source URL: %v", err)
	}

	l.Logf("[BlockList] Updated successfully: %d bytes saved to %s", n, destPath)

	if err := l.Install(); err == nil {
		l.Logf("[BlockList] Live-reloaded into running engine")
	} else {
		l.Logf("[BlockList] WARNING: downloaded but failed to reload live: %v", err)
	}
}
