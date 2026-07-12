package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tiramisu/internal/gostorm/log"

	"tiramisu/internal/gostorm/settings"

	"github.com/anacrolix/torrent/iplist"
)

func ReadBlockedIP() (ranger iplist.Ranger, err error) {
	// 1. Try executable directory first (new auto-update location)
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		buf, err := os.ReadFile(filepath.Join(exeDir, "blocklist"))
		if err == nil {
			log.TLogln("Read block list from binary directory...")
			return parseBlockList(buf)
		}
	}

	// 2. Fallback to settings.Path
	buf, err := os.ReadFile(filepath.Join(settings.Path, "blocklist"))
	if err != nil {
		return nil, err
	}
	log.TLogln("Read block list from settings directory...")
	return parseBlockList(buf)
}

func parseBlockList(buf []byte) (iplist.Ranger, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	var ranges []iplist.Range
	lineCount := 0
	errorCount := 0
	for scanner.Scan() {
		lineCount++
		r, ok, err := iplist.ParseBlocklistP2PLine(scanner.Bytes())
		if err != nil {
			errorCount++
			continue // Skip malformed lines
		}
		if ok {
			ranges = append(ranges, r)
		}
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
		log.TLogln(fmt.Sprintf("Readed ranges: %d (Total lines: %d, Errors: %d)", len(ranges), lineCount, errorCount))
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
