// Package piecedrop drops the copies of a piece that failed its hash. In responsive mode the
// read path serves and caches bytes before the piece is verified; when the hash fails, those
// copies (read-ahead cache, SSD head and tail) must go, or the bad bytes are served again.
package piecedrop

import "time"

// Deps wires the caches. Every func must be safe for concurrent use.
type Deps struct {
	// Files lists the virtual paths open on a torrent file and the file's size.
	Files      func(hash string, fileID int) (paths []string, size int64)
	DropCache  func(path string, off, end int64)
	DropWarmup func(hash string, fileID int, off, end, size int64)
	Rollback   func(hash string, fileID int, off int64) // the stall notice's view of what was served
	Logf       func(format string, args ...any)
	// Settle bounds a fill already holding the bad bytes (the read path's fetch timeout): the
	// copies are dropped again once it has landed.
	Settle time.Duration
}

// Handler returns the engine's corruption hook (engine.SetOnCorrupt).
func Handler(d Deps) func(hash string, fileID int, off, end int64) {
	return func(hash string, fileID int, off, end int64) {
		drop := func() {
			paths, size := d.Files(hash, fileID)
			for _, p := range paths {
				d.DropCache(p, off, end)
			}
			if d.DropWarmup != nil {
				d.DropWarmup(hash, fileID, off, end, size)
			}
			if d.Rollback != nil {
				d.Rollback(hash, fileID, off)
			}
		}
		drop()
		if d.Logf != nil {
			d.Logf("[PieceDrop] %s/%d: bytes %d-%d failed their hash, dropped from the caches", hash, fileID, off, end)
		}
		if d.Settle > 0 {
			time.AfterFunc(d.Settle, drop)
		}
	}
}
