package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tiramisu/internal/cache"
	"tiramisu/internal/library"
	"tiramisu/internal/vfs"
)

// StartupCacheBuilder pre-populates metadata cache at startup
// This dramatically improves Plex scan performance (30s -> 10s for 1000+ files)
// Runs in background to avoid delaying mount operation
type StartupCacheBuilder struct {
	sourcePath string
	metaCache  *cache.LRUCache
	logger     *log.Logger

	// Statistics
	mu             sync.Mutex
	filesProcessed int
	filesSkipped   int
	errors         int
	startTime      time.Time
	foundFiles     map[string]bool // V142: Track files for GC
}

// NewStartupCacheBuilder creates a new startup cache builder
func NewStartupCacheBuilder(sourcePath string, metaCache *cache.LRUCache, logger *log.Logger) *StartupCacheBuilder {
	return &StartupCacheBuilder{
		sourcePath: sourcePath,
		metaCache:  metaCache,
		logger:     logger,
		startTime:  time.Now(),
		foundFiles: make(map[string]bool),
	}
}

// Start begins background cache population
// Returns immediately, processing happens in goroutine
func (b *StartupCacheBuilder) Start() {
	b.logger.Printf("Starting cache pre-population from %s", b.sourcePath)

	// Synchronous, and ahead of the goroutine: an install that predates audio has
	// no music/ or audiobooks/ on disk, and a scanner reaching the mount before
	// they exist sees a missing directory rather than an empty one. Not fatal --
	// a source path this cannot use will fail louder elsewhere.
	if err := library.EnsureSectionRoots(b.sourcePath); err != nil {
		b.logger.Printf("Audio: cannot ensure section roots under %s: %v", b.sourcePath, err)
		b.incrementErrors()
	}

	go func() {
		// Process movies directory
		moviesPath := filepath.Join(b.sourcePath, "movies")
		if _, err := os.Stat(moviesPath); err == nil {
			b.processDirectory(moviesPath, false) // Not recursive
		}

		// Process TV directory (recursive for seasons)
		tvPath := filepath.Join(b.sourcePath, "tv")
		if _, err := os.Stat(tvPath); err == nil {
			b.processDirectory(tvPath, true) // Recursive
		}

		// Audio is driven by the projection registry rather than by walking the
		// section: an unknown file under music/ is not adopted just for being there.
		b.reconcileAudio()

		// Log final statistics
		duration := time.Since(b.startTime)
		b.mu.Lock()
		processed := b.filesProcessed
		skipped := b.filesSkipped
		errors := b.errors
		b.mu.Unlock()

		b.logger.Printf("Cache pre-population complete: %d processed, %d skipped, %d errors in %v",
			processed, skipped, errors, duration)

		// Log cache statistics
		stats := b.metaCache.Stats()
		b.logger.Printf("Cache after startup: %d entries, %.2f MB used of %.2f MB capacity",
			stats.Entries, float64(stats.Size)/(1024*1024), float64(stats.Capacity)/(1024*1024))

		// V133: Save inode map after startup scan completes
		// This ensures the map is persisted even if no background save triggered
		if globalInodeMap != nil && globalInodeMap.IsDirty() {
			if err := globalInodeMap.SaveToDisk(); err != nil {
				b.logger.Printf("InodeMap: Post-startup save error: %v", err)
			} else {
				files, dirs, _, _ := GetInodeMapStats()
				b.logger.Printf("InodeMap: Saved after startup scan (%d files, %d dirs)", files, dirs)
			}
		}

		// V142: Inode GC - Remove ghost entries (files that no longer exist)
		if globalInodeMap != nil {
			b.mu.Lock()
			// Make a copy or pass reference? Pass reference since we are done modifying it
			// However, PruneMissing only reads from it, so it's safe if we don't modify it anymore.
			// We hold b.mu just to be safe or we can just pass it as we are in the final serial block.
			foundSet := b.foundFiles
			b.mu.Unlock()

			pruned := globalInodeMap.PruneMissing(foundSet)
			if pruned > 0 {
				b.logger.Printf("Startup GC: Pruned %d ghost inodes", pruned)
			}
		}
	}()
}

// processDirectory walks directory and processes all .mkv files
// If recursive is true, also processes subdirectories
func (b *StartupCacheBuilder) processDirectory(dirPath string, recursive bool) {
	// Semaphore to limit concurrent file operations
	// Too many concurrent reads can overwhelm the filesystem
	sem := make(chan struct{}, 10) // Max 10 concurrent
	var wg sync.WaitGroup

	// Walk directory
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			b.logger.Printf("Startup cache: error accessing %s: %v", path, err)
			b.incrementErrors()
			return nil // Continue walking
		}

		// Skip directories if not recursive
		if info.IsDir() {
			if !recursive && path != dirPath {
				return filepath.SkipDir
			}
			// V133: Register directory in inode map, keyed by the full path the FUSE
			// call sites look up — a relative key leaves an entry nobody reads.
			if path != b.sourcePath {
				getDirInodeFromMap(path)
			}
			return nil
		}

		// Process only .mkv files
		if filepath.Ext(path) != ".mkv" {
			return nil
		}

		// Check if already in cache (fast path)
		if _, ok := b.metaCache.Get(path); ok {
			b.incrementSkipped()
			return nil
		}

		// Acquire semaphore
		wg.Add(1)
		sem <- struct{}{}

		// Process file in goroutine
		go func(filePath string) {
			defer wg.Done()
			defer func() { <-sem }()

			b.processFile(filePath)
		}(path)

		return nil
	})

	if err != nil {
		b.logger.Printf("Startup cache: error walking %s: %v", dirPath, err)
		b.incrementErrors()
	}

	// Wait for all goroutines to complete
	wg.Wait()
}

// processFile reads metadata for a single file and adds to cache
func (b *StartupCacheBuilder) processFile(path string) {
	// Read metadata from file
	fileMeta, err := vfs.ReadMetadataFromFile(path)
	if err != nil {
		b.logger.Printf("Startup cache: error reading metadata for %s: %v", path, err)
		b.incrementErrors()
		return
	}

	// Convert to Metadata format
	meta := &vfs.Metadata{
		URL:    fileMeta.URL,
		Size:   fileMeta.Size,
		Mtime:  fileMeta.Mtime,
		Path:   fileMeta.Path,
		ImdbID: fileMeta.ImdbID,
	}

	// Calculate approximate size
	size := approximateMetadataSize(meta)

	// Add to cache
	b.metaCache.Put(path, meta, size)

	// V133: Add to inode map for deterministic inode generation
	// This ensures Plex sees the same inode after restarts
	// BUG FIX: Pass full path instead of just filename to avoid collisions
	addFileToInodeMap(path, fileMeta.URL)

	b.incrementProcessed(path)
}

// Statistics helpers (thread-safe)
func (b *StartupCacheBuilder) incrementProcessed(path string) {
	b.mu.Lock()
	b.filesProcessed++
	b.foundFiles[path] = true // V142: Mark as found
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementSkipped() {
	b.mu.Lock()
	b.filesSkipped++
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementErrors() {
	b.mu.Lock()
	b.errors++
	b.mu.Unlock()
}

// reconcileAudio rebuilds the VFS state the committed audio registry implies and
// reports what it could not account for. Registered projections are counted as
// found so the inode GC below does not prune them, and their hashes are kept so
// cleanup can see that a torrent is still projected.
func (b *StartupCacheBuilder) reconcileAudio() {
	if stateDB == nil || globalInodeMap == nil {
		// No authority source at all, so no projection can exist and the audio tree
		// is legitimately empty. This must be Unavailable, not Unready: readers now
		// block on Unreconciled, and an install with a music/ directory and StateDB
		// disabled would hang on its first listing forever.
		//
		// Logged because "unavailable" is otherwise indistinguishable from "empty"
		// to an operator: audio directories simply list nothing, with no clue that
		// the registry was never consulted.
		b.logger.Printf("Audio: no projection registry (state DB unavailable), audio sections serve empty listings")
		globalAudioNamespace.MarkUnavailable()
		return
	}

	// Transactions that crashed between the final rename and the registry commit are
	// rolled back before anything reads committed rows or publishes the namespace.
	//
	// Known transient window, next to the namespace publish race: this pass runs while
	// the HTTP server may already accept requests, and the prune below can remove a
	// directory a live add created but has not filled yet. That add then fails with a
	// spurious 5xx and self-heals at the next boot (its rows stay staged). Tolerated
	// until startup and live adds are serialized.
	if recovered, err := library.RecoverStagedAudioTransactions(stateDB, b.sourcePath, b.logger); err != nil {
		b.logger.Printf("Audio recovery failed: %v", err)
		b.incrementErrors()
	} else if recovered > 0 {
		b.logger.Printf("Audio recovery: rolled back %d staged transaction(s)", recovered)
	}

	// Removals that crashed after the removal mark are finishing work, not rollback:
	// the stub goes and the row follows, so the path and the torrent reference stop
	// being held by a dead projection.
	if swept, err := library.RecoverRemovingAudioProjections(stateDB, b.sourcePath, b.logger); err != nil {
		b.logger.Printf("Audio removal recovery failed: %v", err)
		b.incrementErrors()
	} else if swept > 0 {
		b.logger.Printf("Audio recovery: finished %d interrupted removal(s)", swept)
	}

	result, err := vfs.ReconcileAudio(stateDB, globalInodeMap, b.sourcePath)
	if err != nil {
		b.logger.Printf("Audio reconciliation failed: %v", err)
		b.incrementErrors()
		globalAudioNamespace.MarkFailed(err)
		return
	}

	// Only what the pass above accepted is cached. Reconciliation already rejected
	// the unhealthy rows; caching them here would hand Open the very stub the
	// registry disowned.
	unhealthy := make(map[string]bool, len(result.MissingStub)+len(result.SizeMismatch))
	for _, key := range result.MissingStub {
		unhealthy[key] = true
	}
	for _, key := range result.SizeMismatch {
		unhealthy[key] = true
	}

	// The namespace the VFS dispatches on: only what this pass accepted. Built
	// before it is published so a scanner never sees a half-filled library.
	var committed []library.AudioProjection

	for _, section := range []string{"music", "audiobooks"} {
		projections, err := stateDB.CommittedAudioProjections(section)
		if err != nil {
			// A partial namespace published as complete is worse than none: a
			// scanner would read the missing half as deletions. Leave it unready.
			b.logger.Printf("Audio reconciliation: cannot list %s, audio stays unavailable: %v", section, err)
			b.incrementErrors()
			globalAudioNamespace.MarkFailed(err)
			return
		}
		for _, p := range projections {
			if unhealthy[p.Section+"/"+p.VirtualPath] {
				continue
			}
			committed = append(committed, library.AudioProjection{
				Section: library.Section(p.Section), VirtualPath: p.VirtualPath,
				Hash: p.Hash, FileIndex: p.FileIndex, Size: p.Size, MtimeNS: p.MtimeNS,
				UpdatedAtNS: p.UpdatedAtNS,
				ExternalID:  p.ExternalID, ExternalIDNamespace: p.ExternalIDNamespace,
				CueTrack: p.CueTrack, ByteOffset: p.ByteOffset, Header: p.Header,
			})
			path := filepath.Join(b.sourcePath, p.Section, filepath.FromSlash(p.VirtualPath))
			meta, err := vfs.ReadMetadataFromFileWithLimits(path, vfs.AudioSizeLimits)
			if err != nil {
				continue
			}
			cached := &vfs.Metadata{
				URL: meta.URL, Size: meta.Size, Mtime: meta.Mtime, Path: meta.Path, ImdbID: meta.ImdbID,
			}
			b.metaCache.Put(path, cached, approximateMetadataSize(cached))
			b.mu.Lock()
			b.foundFiles[path] = true
			b.mu.Unlock()
		}
	}

	// Publishing marks the namespace ready. Until this point audio enumeration
	// fails rather than returning an empty directory, which a scanner would read
	// as the user having deleted their library (spec 9).
	//
	// Merged, not replaced: a live add committed between this pass's registry read
	// and this publish is absent from committed, and replacing would drop it from
	// the mount although its row and its stub are both on disk.
	globalAudioNamespace.PublishMerged(committed)
	invalidateAudioDirCaches(committed)

	b.logger.Printf("Audio reconciliation: %d projection(s) registered, %d missing stub(s), %d size mismatch(es), %d torrent(s) referenced",
		result.Registered, len(result.MissingStub), len(result.SizeMismatch), len(result.ReferencedHashes))
	for _, p := range result.MissingStub {
		b.logger.Printf("Audio reconciliation: committed projection has no stub: %s", p)
	}
	for _, p := range result.SizeMismatch {
		b.logger.Printf("Audio reconciliation: stub disagrees with the registry: %s", p)
	}
}

// invalidateAudioDirCaches drops the cached listings for every directory holding a
// committed projection, plus the section roots. Without this, an empty listing
// cached while the namespace was unready would survive publication for the whole
// dircache TTL and a scan in that window would still see nothing.
func invalidateAudioDirCaches(committed []library.AudioProjection) {
	if globalDirCache == nil {
		return
	}
	dirs := map[string]bool{}
	for _, section := range []string{"music", "audiobooks"} {
		dirs[filepath.Join(physicalSourcePath, section)] = true
	}
	for _, p := range committed {
		dir := filepath.Dir(filepath.Join(physicalSourcePath, string(p.Section), filepath.FromSlash(p.VirtualPath)))
		for dir != physicalSourcePath && dir != "." && dir != string(filepath.Separator) {
			if dirs[dir] {
				break
			}
			dirs[dir] = true
			dir = filepath.Dir(dir)
		}
	}
	for dir := range dirs {
		globalDirCache.Delete(dir)
	}
}
