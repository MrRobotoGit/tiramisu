package app

import (
	_ "net/http/pprof"
	"path/filepath"
	"strings"
	"time"
	"tiramisu/internal/webhook"
)

// exactMatchEntry is one playbackRegistry entry: a virtual path and its state.
type exactMatchEntry struct {
	path  string
	state *PlaybackState
}

// exactMatchKeys carries the webhook-side keys used by pass-1 matching.
type exactMatchKeys struct {
	imdbID     string
	identities webhook.IdentityKeys
	basenames  []string
}

// findExactMatch selects the pass-1 match among the registry entries.
//
// The hash suffix is deliberately NOT a key: every episode of a season pack shares
// the same hash8 in its filename (HashPrefix = hash[:8]), so a suffix-only match can
// bind the wrong episode — e.g. the media.stop of E01 killing the pump of E02 that
// is actually playing. The filename (physical identity) wins over the IMDB ID
// (metadata identity), which also protects against an IMDB id polluted by a
// previous mis-matched media.play.
func findExactMatch(keys exactMatchKeys, entries []exactMatchEntry) (string, *PlaybackState) {
	for _, e := range entries {
		if e.state == nil {
			continue
		}
		base := filepath.Base(e.path)
		for _, want := range keys.basenames {
			if want == base {
				return e.path, e.state
			}
		}
	}
	if keys.imdbID != "" {
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			if imdb := e.state.GetImdbID(); imdb != "" && imdb == keys.imdbID {
				return e.path, e.state
			}
		}
	}
	// Audio identity, most specific first: a session registered with the payload's
	// recording id beats one that only shares its release group, which in turn beats
	// one sharing the artist. Within one rank the most recent sign of life wins — the
	// coarser the shared id, the weaker that tiebreak is, which is why an exact
	// recording match is preferred over a recent artist match.
	if !keys.identities.Empty() {
		bestRank := -1
		var bestPath string
		var bestState *PlaybackState
		var bestAt time.Time
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			id, ns := e.state.GetExternalIdentity()
			if id == "" {
				continue
			}
			if ns != "" && !strings.EqualFold(ns, "musicbrainz") {
				continue
			}
			rank, ok := keys.identities.Rank(id)
			if !ok {
				continue
			}
			sign := e.state.LastSignOfLife()
			if rank > bestRank || (rank == bestRank && (bestState == nil || sign.After(bestAt))) {
				bestPath, bestState, bestAt, bestRank = e.path, e.state, sign, rank
			}
		}
		if bestState != nil {
			return bestPath, bestState
		}
	}
	return "", nil
}

// snapshotPlaybackEntries copies the registry into a slice: both pass-1 scans within
// one call see the same entries and order, independent of concurrent registry writes.
func snapshotPlaybackEntries() []exactMatchEntry {
	entries := make([]exactMatchEntry, 0, 16)
	playbackRegistry.Range(func(key, value interface{}) bool {
		entries = append(entries, exactMatchEntry{path: key.(string), state: value.(*PlaybackState)})
		return true
	})
	return entries
}

// anyLiveHandleFor reports whether a FUSE handle for path is still registered. It reads
// activeHandles (the live source of truth) instead of globalOpenTracker on purpose: the
// tracker is not decremented when media.stop deletes the pump before Release, so it can
// latch a stale count and would block stops forever.
func anyLiveHandleFor(path string) bool {
	found := false
	activeHandles.Range(func(key, _ interface{}) bool {
		if key.(*MkvHandle).path == path {
			found = true
			return false
		}
		return true
	})
	return found
}

// hashInPlayback reports whether a confirmed playback is reading a file of the torrent.
func hashInPlayback(hash string) bool {
	found := false
	playbackRegistry.Range(func(_, val interface{}) bool {
		ps := val.(*PlaybackState)
		ps.mu.RLock()
		found = ps.Hash == hash && ps.IsHealthy
		ps.mu.RUnlock()
		return !found
	})
	return found
}

// hashPlaying reports whether a file of the torrent is in playback, confirmed by a webhook or
// inferred from its reads (players without webhooks).
func hashPlaying(hash string) bool {
	found := false
	playbackRegistry.Range(func(_, val interface{}) bool {
		ps := val.(*PlaybackState)
		if ps.Hash == hash && (ps.GetStatus() || ps.IsInferredPlayback()) {
			found = true
		}
		return !found
	})
	return found
}

// anyLiveHandleForHashExcept is anyLiveHandleForHash excluding one path: the file being
// stopped still has its own handle open at webhook time and must not count as a sibling.
func anyLiveHandleForHashExcept(hash, excludePath string) bool {
	if hash == "" {
		return false
	}
	found := false
	activeHandles.Range(func(key, _ interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == excludePath {
			return true
		}
		h.mu.Lock()
		hashOf := h.hash
		h.mu.Unlock()
		if hashOf == hash {
			found = true
			return false
		}
		return true
	})
	return found
}

// shouldDropTorrentPriority reports whether a media.stop may deactivate priority for
// the torrent: never while a file other than the one being stopped still has an open
// handle on the same hash (season-pack episodes share it, so priority-off would hit
// the live stream).
func shouldDropTorrentPriority(hash, excludePath string) bool {
	return hash != "" && !anyLiveHandleForHashExcept(hash, excludePath)
}
