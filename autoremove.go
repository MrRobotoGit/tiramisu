package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/gostorm/native"
	"tiramisu/internal/library"
	"tiramisu/internal/vfs"
)

// TorrentRemover handles automatic torrent removal from GoStorm
// Extracts hash directly from .mkv file content and handles blacklisting
// TorrentRemover handles automatic torrent removal from GoStorm
// Extracts hash directly from .mkv file content and handles blacklisting
type TorrentRemover struct {
	nativeBridge *native.NativeClient // V160: Native Bridge
	logger       *log.Logger
	hashPattern  *regexp.Regexp // Still useful for fallback
}

// NewTorrentRemover creates a new torrent remover
func NewTorrentRemover(nativeBridge *native.NativeClient, logger *log.Logger) *TorrentRemover {
	return &TorrentRemover{
		nativeBridge: nativeBridge,
		logger:       logger,
		hashPattern:  regexp.MustCompile(`(?i)([a-f0-9]{40})`),
	}
}

// RemoveTorrentFromFile extracts hash from file, blacklists it and removes the torrent.
// blacklisted reports whether the release really reached the blacklist.
func (tr *TorrentRemover) RemoveTorrentFromFile(fullPath string) (removed, blacklisted bool, err error) {
	filename := filepath.Base(fullPath)
	title := tr.deriveTitleFromPath(fullPath)

	// V271: Extract hash from filename suffix FIRST (zero I/O, no FUSE access).
	// Previously read the file content first, which could deadlock if the file
	// was being streamed via FUSE (smbd holds read lock → Unlink blocks).
	var fullHash string
	suffixPattern := regexp.MustCompile(`(?i)_([a-f0-9]{8})\.mkv$`)
	if matches := suffixPattern.FindStringSubmatch(filename); len(matches) > 1 {
		fullHash, _, _ = tr.findFullHashBySuffix(matches[1])
	}

	// Fallback: read hash from file content (only if suffix lookup failed)
	if fullHash == "" {
		fullHash = tr.extractHashFromFile(fullPath)
	}

	// CRITICAL FIX: Ensure hash is valid and 40 chars before proceeding
	fullHash = strings.ToLower(fullHash)
	if len(fullHash) != 40 {
		return false, false, fmt.Errorf("invalid or missing hash (%s) for: %s", fullHash, filename)
	}

	tr.logger.Printf("AutoRemove: identified hash %s for %s", fullHash[:8], title)

	// 2. ALWAYS add to blacklist (FASE 5.1)
	blacklisted = true
	if err := tr.addToBlacklist(fullHash, title); err != nil {
		blacklisted = false
		tr.logger.Printf("AutoRemove: WARNING: %s not blacklisted, the sync may add it back: %v", fullHash[:8], err)
	}

	// 3. Attempt to remove from GoStorm
	removed, err = tr.removeTorrent(fullHash)
	if err != nil {
		tr.logger.Printf("AutoRemove: removal from GoStorm failed for %s: %v", fullHash[:8], err)
		return false, blacklisted, nil
	}

	return removed, blacklisted, nil
}

// extractHashFromFile reads the virtual mkv to get the hash.
// Supports both JSON (new) and line-based (legacy) formats.
func (tr *TorrentRemover) extractHashFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	trimmed := strings.TrimSpace(content)

	// Detect JSON format
	if strings.HasPrefix(trimmed, "{") {
		var j vfs.MkvJSON
		if err := json.Unmarshal([]byte(content), &j); err != nil {
			return ""
		}
		match := tr.hashPattern.FindString(j.URL)
		return strings.ToLower(match)
	}

	// Legacy line-based: read first line and extract hash
	scanner := bufio.NewScanner(strings.NewReader(content))
	if scanner.Scan() {
		line := scanner.Text()
		match := tr.hashPattern.FindString(line)
		return strings.ToLower(match)
	}
	return ""
}

// deriveTitleFromPath gets the show or movie name from the directory structure
func (tr *TorrentRemover) deriveTitleFromPath(path string) string {
	// Example: /mnt/torrserver/tv/9-1-1_Nashville (2025)/Season.01/file.mkv
	// We want "9-1-1_Nashville (2025)"
	parts := strings.Split(filepath.Dir(path), string(os.PathSeparator))

	// Default to filename if path is shallow
	if len(parts) < 2 {
		return filepath.Base(path)
	}

	// If it's a TV show (contains "Season.XX"), take the parent
	lastDir := parts[len(parts)-1]
	if strings.HasPrefix(lastDir, "Season.") && len(parts) >= 2 {
		return parts[len(parts)-2]
	}

	return lastDir
}

// addToBlacklist adds a hash and title to the persistent blacklist file
func (tr *TorrentRemover) addToBlacklist(hash, title string) error {
	return tr.addToBlacklistAt(filepath.Join(GetStateDir(), "blacklist.json"), hash, title)
}

// blacklistMu serialises the read-modify-write: the unlink handler, the Library API
// and the reaper write concurrently, and the last rename would drop the others' hashes.
var blacklistMu sync.Mutex

func (tr *TorrentRemover) addToBlacklistAt(blacklistPath, hash, title string) error {
	blacklistMu.Lock()
	defer blacklistMu.Unlock()
	// Readers look the hash up lowercased, so a capitalised entry would never match.
	hash = strings.ToLower(hash)

	type Blacklist struct {
		Hashes map[string]string `json:"hashes"`
		Titles []string          `json:"titles"`
	}

	var bl Blacklist
	data, err := ioutil.ReadFile(blacklistPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &bl); err != nil {
			// Set aside, never rewritten in place: the bytes stay for a repair, and the
			// readers already see an empty blacklist while the file is unparsable.
			aside := fmt.Sprintf("%s.corrupt-%d", blacklistPath, time.Now().UnixNano())
			if rerr := os.Rename(blacklistPath, aside); rerr != nil {
				return fmt.Errorf("blacklist %s is unreadable (%v) and cannot be set aside: %w", blacklistPath, err, rerr)
			}
			tr.logger.Printf("BLACKLIST WARNING: %s was unreadable (%v), moved to %s; starting a new one: every release blacklisted before is no longer excluded until that file is repaired and merged back", blacklistPath, err, aside)
			bl = Blacklist{}
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("read blacklist: %w", err)
	}
	if bl.Hashes == nil {
		bl.Hashes = make(map[string]string)
	}
	if bl.Titles == nil {
		bl.Titles = []string{}
	}

	changed := false
	if _, exists := bl.Hashes[hash]; !exists {
		bl.Hashes[hash] = title
		changed = true
	}

	// SUPER-CLEAN Title Normalization (for robust matching)
	if title != "" {
		// 1. Convert to lowercase
		cleanTitle := strings.ToLower(title)
		// 2. Replace underscores and dots with spaces
		cleanTitle = strings.ReplaceAll(cleanTitle, "_", " ")
		cleanTitle = strings.ReplaceAll(cleanTitle, ".", " ")
		// 3. Remove year (e.g., "(2023)" or "2023")
		yearRegex := regexp.MustCompile(`\(?\d{4}\)?`)
		cleanTitle = yearRegex.ReplaceAllString(cleanTitle, "")
		// 4. Remove ALL non-alphanumeric characters AND spaces (SQUEEZE everything)
		// This ensures "Grey's Anatomy" becomes "greysanatomy" and "9-1-1" becomes "911"
		nonAlphaRegex := regexp.MustCompile(`[^a-z0-9]`)
		cleanTitle = nonAlphaRegex.ReplaceAllString(cleanTitle, "")

		if cleanTitle != "" {

			found := false
			for _, t := range bl.Titles {
				if t == cleanTitle {
					found = true
					break
				}
			}
			if !found {
				bl.Titles = append(bl.Titles, cleanTitle)
				changed = true
			}
		}
	}

	if !changed {
		return nil
	}
	newData, err := json.MarshalIndent(bl, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := blacklistPath + ".tmp"
	if err := ioutil.WriteFile(tmpPath, newData, 0644); err != nil {
		return fmt.Errorf("write blacklist: %w", err)
	}
	if err := os.Rename(tmpPath, blacklistPath); err != nil {
		return fmt.Errorf("write blacklist: %w", err)
	}
	tr.logger.Printf("BLACKLISTED: %s (%s)", hash[:8], title)
	return nil
}

// findFullHashBySuffix (Fallback logic)
func (tr *TorrentRemover) findFullHashBySuffix(suffix string) (string, string, error) {
	// Use Native Bridge to list torrents (O(N) operation but in-memory so fast)
	torrents, err := tr.nativeBridge.ListTorrents()
	if err != nil {
		return "", "", err
	}

	for _, t := range torrents {
		if strings.HasSuffix(strings.ToLower(t.Hash), strings.ToLower(suffix)) {
			return t.Hash, t.Title, nil
		}
	}
	return "", "", nil
}

// removeTorrent calls GoStorm API to remove a torrent
func (tr *TorrentRemover) removeTorrent(hash string) (bool, error) {
	// Unlinking a video stub must not take a torrent an audio projection is still
	// projected from: audio lives in the registry, not as a file under the video
	// directories, so nothing above this point can see it. Fails closed.
	registry := audioOwnershipRegistry()
	allowed, err := library.MayDropTorrent(registry, hash)
	if err != nil {
		tr.logger.Printf("AutoRemove: keeping torrent %s, cannot check its audio projections: %v", hash[:8], err)
		return false, nil
	}
	if !allowed {
		tr.logger.Printf("AutoRemove: keeping torrent %s, an audio projection still references it", hash[:8])
		return false, nil
	}

	// Use Native Bridge to remove torrent
	tr.nativeBridge.RemoveTorrent(hash)

	if globalSyncCacheManager != nil {
		globalSyncCacheManager.ClearNegativeCache(hash)
	}
	return true, nil
}
