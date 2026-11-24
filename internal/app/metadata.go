package app

import (
	_ "net/http/pprof"
	"tiramisu/internal/vfs"
)

// approximateMetadataSize estimates the memory footprint of a Metadata entry
// Used for LRU cache size tracking
func approximateMetadataSize(m *vfs.Metadata) int64 {
	// Approximate size:
	// - URL string: len(URL)
	// - Path string: len(Path)
	// - Size: 8 bytes (int64)
	// - Mtime: 24 bytes (time.Time structure)
	// - Overhead: 64 bytes (pointers, struct alignment)
	return int64(len(m.URL) + len(m.Path) + 8 + 24 + 64)
}

func getOrReadMeta(path string) (*vfs.Metadata, error) {
	var m *vfs.Metadata

	// Check cache first (fast path without lock)
	if val, ok := metaCache.Get(path); ok {
		m = val
	} else {
		// Acquire lock to prevent stampede
		unlock := globalLockManager.Lock(path)
		defer unlock()

		// Double-check cache after acquiring lock
		if val, ok := metaCache.Get(path); ok {
			m = val
		} else {
			fileMeta, err := vfs.ReadMetadataFromFileWithLimits(path, stubSizeLimits(path))
			if err != nil {
				return nil, err
			}

			m = &vfs.Metadata{
				URL:                 fileMeta.URL,
				Size:                fileMeta.Size,
				Mtime:               fileMeta.Mtime,
				Path:                fileMeta.Path,
				ImdbID:              fileMeta.ImdbID,
				ExternalID:          fileMeta.ExternalID,
				ExternalIDNamespace: fileMeta.ExternalIDNamespace,
				Audio:               isAudioSectionPath(path),
			}

			metaCache.Put(path, m, approximateMetadataSize(m))
		}
	}

	return m, nil
}
