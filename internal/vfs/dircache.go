package vfs

import (
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// DirCacheEntry holds cached directory entries
type DirCacheEntry struct {
	Entries   []fuse.DirEntry
	ExpiresAt time.Time
}

// DirCache provides a thread-safe cache for directory listings
type DirCache struct {
	cache map[string]DirCacheEntry
	// generations counts invalidations per path. A reader snapshots it before walking
	// the directory and only stores the listing if no Delete landed meanwhile: without
	// it, a cache-miss Readdir that lost the race writes a stale listing back after
	// the invalidation and the entry survives the whole TTL.
	generations map[string]uint64
	mu          sync.RWMutex
	ttl         time.Duration
}

// NewDirCache creates a new directory cache
func NewDirCache(ttl time.Duration) *DirCache {
	return &DirCache{
		cache:       make(map[string]DirCacheEntry),
		generations: make(map[string]uint64),
		ttl:         ttl,
	}
}

// Get retrieves entries for a path if they exist and aren't expired
func (dc *DirCache) Get(path string) ([]fuse.DirEntry, bool) {
	dc.mu.RLock()
	entry, exists := dc.cache[path]
	dc.mu.RUnlock()

	if !exists {
		return nil, false
	}

	if time.Now().After(entry.ExpiresAt) {
		// Lazy cleanup
		dc.Delete(path)
		return nil, false
	}

	return entry.Entries, true
}

// Generation snapshots a path's invalidation counter, to be handed back to
// PutIfGeneration after the directory has been read.
func (dc *DirCache) Generation(path string) uint64 {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.generations[path]
}

// PutIfGeneration stores entries only when the path has not been invalidated since
// the generation was taken. Reports whether the listing was stored.
func (dc *DirCache) PutIfGeneration(path string, entries []fuse.DirEntry, generation uint64) bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.generations[path] != generation {
		return false
	}
	dc.putLocked(path, entries)
	return true
}

// Put stores entries for a path
func (dc *DirCache) Put(path string, entries []fuse.DirEntry) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.putLocked(path, entries)
}

func (dc *DirCache) putLocked(path string, entries []fuse.DirEntry) {
	// Make a copy to prevent mutation issues (though DirEntry is value type, slice is ref)
	entriesCopy := make([]fuse.DirEntry, len(entries))
	copy(entriesCopy, entries)

	dc.cache[path] = DirCacheEntry{
		Entries:   entriesCopy,
		ExpiresAt: time.Now().Add(dc.ttl),
	}
}

// Delete removes an entry and bumps its generation (used for invalidation)
func (dc *DirCache) Delete(path string) {
	dc.mu.Lock()
	delete(dc.cache, path)
	dc.generations[path]++
	dc.mu.Unlock()
}

// CleanupExpired purges expired entries (can be called periodically)
func (dc *DirCache) CleanupExpired() {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	now := time.Now()
	for path, entry := range dc.cache {
		if now.After(entry.ExpiresAt) {
			delete(dc.cache, path)
		}
	}
}

// Global instance
