package app

import (
	_ "net/http/pprof"
	"sync"
	"time"
)

// PlaybackState traccia lo stato di una sessione di visione reale
type PlaybackState struct {
	mu     sync.RWMutex
	Path   string
	Hash   string // InfoHash for Tentacle priority management
	ImdbID string // IMDB ID from MKV line 4, used for webhook matching
	// ExternalID/ExternalIDNamespace carry the audio projection's registered
	// identity ("musicbrainz", "asin", ...) so a music webhook can match the
	// session the same way a movie webhook matches on ImdbID.
	ExternalID          string
	ExternalIDNamespace string
	OpenedAt            time.Time
	ConfirmedAt         time.Time // Set when Plex webhook arrives
	IsHealthy           bool      // Confirmed by Plex
	IsStopped           bool      // Set on explicit media.stop webhook
	stopIgnoredAt       time.Time // media.stop guarded while a handle was open; applied if the last one closes soon after
	// V750: Inferred playback detection (self-healing when webhook is lost)
	ReadCount   int64
	LastSeekOff int64
	LastReadAt  time.Time
}

func (ps *PlaybackState) SetHealthy(healthy bool) {
	ps.mu.Lock()
	ps.IsHealthy = healthy
	ps.ConfirmedAt = time.Now()
	ps.mu.Unlock()
	// V750: Persist to SQLite
	savePlaybackStateToDB(ps)
}

// V750: Determine if this path shows evidence of active playback,
// even without a webhook confirmation.
func (ps *PlaybackState) IsInferredPlayback() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.IsHealthy || ps.IsStopped {
		return false
	}
	now := time.Now()
	active := !ps.LastReadAt.IsZero() && now.Sub(ps.LastReadAt) < 10*time.Minute
	significantReads := ps.ReadCount >= 3
	streaming := ps.LastSeekOff > 2*1024*1024 // >2MB offset = real playback, not scanner
	return active && significantReads && streaming
}

func (ps *PlaybackState) GetOpenedAt() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.OpenedAt
}

func (ps *PlaybackState) GetImdbID() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ImdbID
}

// GetExternalIdentity returns the audio projection identity and its namespace.
func (ps *PlaybackState) GetExternalIdentity() (string, string) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ExternalID, ps.ExternalIDNamespace
}

// LastSignOfLife is the later of the open and the webhook confirmation.
func (ps *PlaybackState) LastSignOfLife() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.ConfirmedAt.After(ps.OpenedAt) {
		return ps.ConfirmedAt
	}
	return ps.OpenedAt
}

// ignoredStopWindow bounds how long a guarded media.stop waits for the last handle to close.
const ignoredStopWindow = 15 * time.Second

// deferredStopGrace: after the last close a guarded stop waits this long and is dropped if the file
// reopens, since Plex closes and reopens mid-playback (V306).
var deferredStopGrace = 5 * time.Second

func (ps *PlaybackState) confirmedAfter(t time.Time) bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ConfirmedAt.After(t)
}

func (ps *PlaybackState) noteIgnoredStop(now time.Time) {
	ps.mu.Lock()
	ps.stopIgnoredAt = now
	ps.mu.Unlock()
}

// takeIgnoredStop consumes a guarded media.stop still within the window and not superseded by a later confirmation.
func (ps *PlaybackState) takeIgnoredStop(now time.Time) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	at := ps.stopIgnoredAt
	ps.stopIgnoredAt = time.Time{}
	return !at.IsZero() && now.Sub(at) <= ignoredStopWindow && !ps.ConfirmedAt.After(at)
}

func (ps *PlaybackState) GetStatus() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.IsHealthy
}

var playbackRegistry sync.Map // path -> *PlaybackState
