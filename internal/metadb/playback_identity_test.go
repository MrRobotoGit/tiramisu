package metadb

import (
	"path/filepath"
	"testing"
	"time"
)

// The audio identity a projection was registered with has to survive a restart on the
// playback state, or a music webhook that arrives after a reboot has nothing to match.
func TestPlaybackExternalIdentityRoundTrip(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "playback-identity.db"))
	now := time.Now().Truncate(time.Microsecond)
	rec := &PlaybackRecord{
		Path:         "/mnt/tiramisu/music/Artist/Album/01 - Track.flac",
		Hash:         "9b3103673dfce0841cb41afa3a9f944f9ac7684d",
		ExternalID:   "b17c61df-f4c8-4213-bbe2-9c82dc852bd9",
		ExternalIDNS: "musicbrainz",
		OpenedAt:     now,
		ConfirmedAt:  now,
		IsHealthy:    true,
		LastReadAt:   now,
		ReadCount:    7,
		LastSeekOff:  65536,
	}
	if err := db.SavePlaybackState(rec); err != nil {
		t.Fatalf("SavePlaybackState: %v", err)
	}

	loaded, err := db.LoadPlaybackStates(time.Hour)
	if err != nil {
		t.Fatalf("LoadPlaybackStates: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadPlaybackStates returned %d records, want 1", len(loaded))
	}
	got := loaded[0]
	if got.ExternalID != rec.ExternalID || got.ExternalIDNS != rec.ExternalIDNS {
		t.Errorf("loaded identity = %q/%q, want %q/%q", got.ExternalIDNS, got.ExternalID, rec.ExternalIDNS, rec.ExternalID)
	}
	if got.ImdbID != "" {
		t.Errorf("loaded IMDb id = %q, want empty for an audio state", got.ImdbID)
	}

	byPath, err := db.LoadPlaybackStateByPath(rec.Path)
	if err != nil {
		t.Fatalf("LoadPlaybackStateByPath: %v", err)
	}
	if byPath == nil || byPath.ExternalID != rec.ExternalID || byPath.ExternalIDNS != rec.ExternalIDNS {
		t.Errorf("by-path identity = %+v, want %q/%q", byPath, rec.ExternalIDNS, rec.ExternalID)
	}
}
