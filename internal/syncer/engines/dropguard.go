package engines

import (
	"context"
	"log"

	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
)

// audioRegistry adapts the state DB to the ownership interface. The explicit nil
// check matters: a nil *metadb.DB stored in an interface is not a nil interface,
// and the guard would call through it.
func audioRegistry(db *metadb.DB) library.AudioRegistry {
	if db == nil {
		return nil
	}
	return db
}

// dropTorrentGuarded removes a torrent unless an audio projection still owns it,
// deriving ownership from the state DB.
func dropTorrentGuarded(ctx context.Context, gs *GoStormClient, db *metadb.DB, logger *log.Logger, hash string) (bool, error) {
	return dropTorrentGuardedWith(ctx, gs, audioRegistry(db), logger, hash)
}

// dropTorrentGuardedWith is the same decision against a caller-supplied registry.
// A nil registry means audio is not configured; an UnavailableAudioRegistry means
// it is configured but could not be read, which is not the same thing and must not
// permit a drop. A sync engine sees only its own media kind on disk, so without
// this it drops a torrent an album is still projected from.
func dropTorrentGuardedWith(ctx context.Context, gs *GoStormClient, reg library.AudioRegistry, logger *log.Logger, hash string) (bool, error) {
	allowed, err := library.MayDropTorrent(reg, hash)
	if err != nil {
		if logger != nil {
			logger.Printf("[Sync] WARNING: keeping torrent %s, cannot check its audio projections: %v", hash, err)
		}
		return false, nil
	}
	if !allowed {
		return false, nil
	}
	if err := gs.RemoveTorrent(ctx, hash); err != nil {
		return false, err
	}
	return true, nil
}

// ownership prefers an explicitly configured registry so the host can express
// "configured but unavailable"; a bare DB is the fallback for callers that only
// set one.
func ownershipOf(reg library.AudioRegistry, db *metadb.DB) library.AudioRegistry {
	if reg != nil {
		return reg
	}
	return audioRegistry(db)
}

func (e *MovieGoEngine) dropTorrent(ctx context.Context, hash string) (bool, error) {
	return dropTorrentGuardedWith(ctx, e.gostorm, ownershipOf(e.audioReg, e.db), e.logger, hash)
}

func (e *TVGoEngine) dropTorrent(ctx context.Context, hash string) (bool, error) {
	return dropTorrentGuardedWith(ctx, e.gostorm, ownershipOf(e.audioReg, e.db), e.logger, hash)
}

func (e *WatchlistGoEngine) dropTorrent(ctx context.Context, hash string) (bool, error) {
	return dropTorrentGuardedWith(ctx, e.gostorm, ownershipOf(e.audioReg, e.db), e.logger, hash)
}
