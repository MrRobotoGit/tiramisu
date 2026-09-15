package cache

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tiramisu/internal/metadb"
)

// SyncCacheManager manages synchronization caches.
// V83: In-memory cache to eliminate disk I/O on every access.
// V1.7.1: SQLite persistence replaces JSON temp+rename writes.
type SyncCacheManager struct {
	stateDir string
	mu       sync.RWMutex
	logger   *log.Logger
	db       *metadb.DB

	// V83: In-memory caches (loaded at startup)
	negativeCache map[string]NegativeCacheEntry
	dirty         bool
}

// NewSyncCacheManager creates a new cache synchronization manager.
func NewSyncCacheManager(stateDir string, logger *log.Logger) *SyncCacheManager {
	return &SyncCacheManager{
		stateDir:      stateDir,
		logger:        logger,
		negativeCache: make(map[string]NegativeCacheEntry),
		dirty:         false,
	}
}

// SetDB enables SQLite persistence. Call after NewSyncCacheManager and before any operations.
func (s *SyncCacheManager) SetDB(db *metadb.DB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

// LoadCachesFromDisk loads cache entries into memory (one-time at startup).
// If SQLite is available, loads from DB; otherwise falls back to JSON files.
func (s *SyncCacheManager) LoadCachesFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		neg, err := s.db.LoadNegatives()
		if err != nil {
			s.logger.Printf("SyncCache: Warning - failed to load from DB: %v", err)
			// Fallback to JSON
			return s.loadFromJSONLocked()
		}
		s.negativeCache = make(map[string]NegativeCacheEntry, len(neg))
		for hash, entry := range neg {
			ts, _ := time.Parse(time.RFC3339, entry.Timestamp)
			s.negativeCache[hash] = NegativeCacheEntry{Hash: hash, Timestamp: ts}
		}
		s.logger.Printf("SyncCache: Loaded %d negative entries from StateDB", len(s.negativeCache))
		return nil
	}

	return s.loadFromJSONLocked()
}

func (s *SyncCacheManager) loadFromJSONLocked() error {
	// Legacy JSON loading (fallback when StateDB is disabled)
	negPath := s.stateDir + "/no_mkv_hashes.json"

	if data, err := readFileSafe(negPath); err == nil {
		if err := unmarshalJSON(data, &s.negativeCache); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to parse negative cache: %v", err)
			s.negativeCache = make(map[string]NegativeCacheEntry)
		}
	} else {
		s.negativeCache = make(map[string]NegativeCacheEntry)
	}

	s.logger.Printf("SyncCache: Loaded %d negative entries from disk", len(s.negativeCache))
	return nil
}

// SyncToDisk refreshes the in-memory view from the state DB, or writes the JSON
// files when there is no DB. With a DB the sync engines own the rows and write them
// at the end of a run: writing this snapshot back would undo whatever they stored
// since startup, so here the DB is read, never overwritten.
func (s *SyncCacheManager) SyncToDisk() error {
	if s.db != nil {
		return s.refreshFromDB()
	}

	s.mu.Lock()

	if !s.dirty {
		s.mu.Unlock()
		return nil
	}

	negCopy := make(map[string]metadb.NegativeCacheEntry, len(s.negativeCache))
	for k, v := range s.negativeCache {
		negCopy[k] = metadb.NegativeCacheEntry{
			Hash:      k,
			Timestamp: v.Timestamp.UTC().Format(time.RFC3339),
		}
	}

	s.dirty = false
	s.mu.Unlock()

	// Fallback: legacy JSON write
	return s.syncJSONLocked(negCopy)
}

// refreshFromDB reloads the counters the dashboard reads. It is the read half of the
// 30s tick: the rows themselves belong to whoever wrote them.
func (s *SyncCacheManager) refreshFromDB() error {
	neg, err := s.db.LoadNegatives()
	if err != nil {
		return fmt.Errorf("read sync caches from DB: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.negativeCache = make(map[string]NegativeCacheEntry, len(neg))
	for hash, entry := range neg {
		ts, _ := time.Parse(time.RFC3339, entry.Timestamp)
		s.negativeCache[hash] = NegativeCacheEntry{Hash: hash, Timestamp: ts}
	}
	s.dirty = false
	return nil
}

// ClearNegativeCache removes a hash from the negative cache.
func (s *SyncCacheManager) ClearNegativeCache(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.negativeCache[hash]; exists {
		delete(s.negativeCache, hash)
		s.dirty = true
		s.logger.Printf("SyncCache: Cleared negative cache for hash %s", hash[:8])
	}
	if s.db != nil {
		return s.db.RemoveNegative(hash)
	}
	return nil
}

// CleanupStaleEntries removes expired entries from all caches. With a DB the delete
// runs there, scoped by timestamp: dropping only what this snapshot considers stale
// would leave behind every row written after startup.
func (s *SyncCacheManager) CleanupStaleEntries(negativeTTL time.Duration) error {
	if s.db != nil {
		removed, err := s.db.CleanupStale(negativeTTL)
		if err != nil {
			return fmt.Errorf("cleanup sync caches in DB: %w", err)
		}
		if removed > 0 {
			s.logger.Printf("SyncCache: Cleaned up %d stale entries", removed)
		}
		return s.refreshFromDB()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0

	for hash, entry := range s.negativeCache {
		if now.Sub(entry.Timestamp) > negativeTTL {
			delete(s.negativeCache, hash)
			removed++
		}
	}

	if removed > 0 {
		s.dirty = true
		s.logger.Printf("SyncCache: Cleaned up %d stale entries", removed)
	}

	return nil
}

// Stats returns cache statistics.
func (s *SyncCacheManager) Stats() SyncCacheStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return SyncCacheStats{
		NegativeCacheEntries: len(s.negativeCache),
	}
}

// SyncCacheStats holds cache statistics.
type SyncCacheStats struct {
	NegativeCacheEntries int
	// FullpackCacheEntries stays 0: the field survives only because /metrics is a
	// published contract, and its consumers would break on a missing key.
	FullpackCacheEntries int
}

// NegativeCacheEntry represents a torrent hash without valid mkv files.
type NegativeCacheEntry struct {
	Hash      string    `json:"hash"`
	Timestamp time.Time `json:"timestamp"`
}

// syncJSONLocked writes caches to JSON files using temp+rename (legacy fallback).
func (s *SyncCacheManager) syncJSONLocked(negCopy map[string]metadb.NegativeCacheEntry) error {
	negPath := filepath.Join(s.stateDir, "no_mkv_hashes.json")
	if err := atomicWriteJSON(negPath, negCopy); err != nil {
		return fmt.Errorf("sync negative cache: %w", err)
	}

	s.logger.Printf("SyncCache: Synced %d negative entries to disk", len(negCopy))
	return nil
}

// readFileSafe reads a file, returning error if not found or unreadable.
func readFileSafe(path string) ([]byte, error) {
	return ioutil.ReadFile(path)
}

// unmarshalJSON unmarshals JSON data into a target.
func unmarshalJSON(data []byte, target interface{}) error {
	return json.Unmarshal(data, target)
}

// atomicWriteJSON writes data to a JSON file atomically using temp file + rename.
func atomicWriteJSON(path string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	dir := filepath.Dir(path)
	tempFile, err := ioutil.TempFile(dir, ".tmp-cache-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	if _, err := tempFile.Write(jsonData); err != nil {
		tempFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}
