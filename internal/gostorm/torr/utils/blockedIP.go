package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"tiramisu/internal/gostorm/log"

	"tiramisu/internal/gostorm/settings"

	"github.com/anacrolix/torrent/iplist"
)

// blockListEnabled gates ReadBlockedIP. Tiramisu owns the setting (blocklist_enabled
// in config.json) and pushes it here at startup and on every change: without the gate
// a leftover blocklist file kept being loaded, so turning the feature off in the
// Control Panel left every range still banned.
var blockListEnabled atomic.Bool

// SetBlockListEnabled turns blocklist loading on or off.
func SetBlockListEnabled(enabled bool) { blockListEnabled.Store(enabled) }

// blockListFilter keeps only the ranges whose description matches. Published lists mix a
// small live section with a large stale one - iblocklist Level 1 carries ~5k current
// anti-P2P entries among ~231k corporate whois records from the 1990s, and loading the
// lot rejects ordinary peers on netblocks that changed hands years ago. Empty means
// "keep everything".
var blockListFilter atomic.Pointer[regexp.Regexp]

// SetBlockListFilter compiles and installs the description filter. An invalid pattern is
// rejected and reported rather than silently dropping every range.
func SetBlockListFilter(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		blockListFilter.Store(nil)
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("blocklist filter %q: %w", pattern, err)
	}
	blockListFilter.Store(re)
	return nil
}

// ReadBlockedIP loads the blocklist file. Returns (nil, nil) when the feature is
// disabled, which callers apply as "no ranges banned".
func ReadBlockedIP() (ranger iplist.Ranger, err error) {
	if !blockListEnabled.Load() {
		return nil, nil
	}

	// 1. The state directory, where the downloader writes. These are the same directory
	// under systemd; in a container only this one is on a mounted volume, so a stale copy
	// beside the binary must not outrank the file being refreshed.
	if settings.Path != "" {
		buf, err := os.ReadFile(filepath.Join(settings.Path, "blocklist"))
		if err == nil {
			log.TLogln("Read block list from settings directory...")
			return parseBlockList(buf)
		}
	}

	// 2. Fall back to the binary's directory.
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(filepath.Join(filepath.Dir(exePath), "blocklist"))
	if err != nil {
		return nil, err
	}
	log.TLogln("Read block list from binary directory...")
	return parseBlockList(buf)
}

func parseBlockList(buf []byte) (iplist.Ranger, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	var ranges []iplist.Range
	lineCount := 0
	errorCount := 0
	filtered := 0
	for scanner.Scan() {
		lineCount++
		r, ok, err := iplist.ParseBlocklistP2PLine(scanner.Bytes())
		if err != nil {
			errorCount++
			continue // Skip malformed lines
		}
		if !ok {
			continue
		}
		if re := blockListFilter.Load(); re != nil && !re.MatchString(r.Description) {
			filtered++
			continue
		}
		ranges = append(ranges, r)
	}

	if err := scanner.Err(); err != nil {
		log.TLogln("Scanner error during blocklist parse:", err)
	}

	if len(ranges) > 0 {
		// iplist.New's Lookup binary-searches by First IP; the file is sorted by
		// description text, not IP, so it must be sorted here or matches get missed.
		sort.Slice(ranges, func(i, j int) bool {
			return bytes.Compare(ranges[i].First, ranges[j].First) < 0
		})
		if filtered > 0 {
			log.TLogln(fmt.Sprintf("Readed ranges: %d (Total lines: %d, Errors: %d, Filtered out: %d)", len(ranges), lineCount, errorCount, filtered))
		} else {
			log.TLogln(fmt.Sprintf("Readed ranges: %d (Total lines: %d, Errors: %d)", len(ranges), lineCount, errorCount))
		}
		return iplist.New(ranges), nil
	}
	// Zero valid ranges from an otherwise-clean scan (no scanner.Err()) must still surface
	// as an error: the caller treats a nil error as "reload succeeded" and would live-swap
	// in a nil blocklist, silently disabling the filter instead of keeping the last-good one.
	if err := scanner.Err(); err != nil {
		log.TLogln(fmt.Sprintf("No ranges loaded from blocklist! (Lines read: %d, Errors: %d)", lineCount, errorCount))
		return nil, err
	}
	log.TLogln(fmt.Sprintf("No ranges loaded from blocklist! (Lines read: %d, Errors: %d)", lineCount, errorCount))
	return nil, fmt.Errorf("no valid ranges parsed (lines read: %d, errors: %d)", lineCount, errorCount)
}

// CountRanges reports how many P2P-format lines the file parses into. Used as a sanity
// gate before a freshly downloaded list replaces the one in use: iblocklist serves a
// captcha page to anything that looks like a browser, and installing that HTML would
// silently leave the engine with no ranges at all.
func CountRanges(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.LastIndex(line, ":"); i >= 0 && strings.Contains(line[i+1:], "-") {
			n++
		}
	}
	return n
}
