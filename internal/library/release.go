package library

import (
	"context"
	"fmt"

	"tiramisu/internal/metadb"
)

// episodesByHash is what *metadb.DB offers beyond EpisodeRegistry: the episodes one
// torrent stands behind, through idx_tv_hash.
type episodesByHash interface {
	EpisodesByHash(hash string) ([]metadb.EpisodeEntry, error)
}

// ReleaseIfUnused deletes a torrent from the engine only when nothing references it any
// more: no movie or TV stub (matched on the full hash read from the stub), no tv_episodes
// row, no audio projection in any state, no add of the same hash in flight. It is the one
// place a library entry's torrent goes, so a season pack is never dropped from under its
// other episodes.
//
// released=false with a nil error means it is still in use, or an add holds it now: the
// orphan collection picks it up later if it really is unused. A check or engine failure is
// an error, never a silent keep.
func (m *Manager) ReleaseIfUnused(ctx context.Context, hash string) (released bool, err error) {
	// A stub that carries no hash identifies no torrent: findByHash would match on a bare
	// "_.mkv" suffix and the engine would be asked to remove the empty hash.
	// Rows, stubs and the add lock all key on the lowercase hex form.
	hash = canonicalHashKey(hash)
	if hash == "" {
		return false, nil
	}
	// Never wait: whoever holds this hash is adding that same release right now.
	unlock, ok := m.hashLocks.TryLock(hash)
	if !ok {
		return false, nil
	}
	defer unlock()

	for _, kind := range []string{"movie", "tv"} {
		found, err := m.findByHash(kind, hash)
		if err != nil {
			return false, fmt.Errorf("cannot check its %s stubs: %w", kind, err)
		}
		for _, f := range found {
			// findByHash matches on the 8 characters in the name; the stub has the full hash.
			if st := readStub(f.Path); st.Hash == "" || diagHash(st.Hash) == diagHash(hash) {
				return false, nil
			}
		}
	}
	// A registry row without its stub (a removal cut short) still claims the torrent.
	if r, ok := m.cfg.Registry.(episodesByHash); ok {
		eps, err := r.EpisodesByHash(hash)
		if err != nil {
			return false, fmt.Errorf("cannot check its episodes: %w", err)
		}
		if len(eps) > 0 {
			return false, nil
		}
	}
	// Audio lives in the projection registry, not as a stub: staged and removing rows count
	// too, since a removal in flight may have a reader holding the file open.
	if m.cfg.AudioRegistry != nil {
		referenced, err := m.cfg.AudioRegistry.AudioHashReferenced(hash)
		if err != nil {
			return false, fmt.Errorf("cannot check its audio projections: %w", err)
		}
		if referenced {
			return false, nil
		}
	}
	if err := m.removeTorrent(ctx, hash); err != nil {
		return false, err
	}
	if m.cfg.OnRelease != nil {
		m.cfg.OnRelease(hash)
	}
	return true, nil
}
