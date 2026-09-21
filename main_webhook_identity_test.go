package main

import (
	"testing"
	"time"
)

func TestWebhookIdentityKeysRanking(t *testing.T) {
	const (
		recording = "b17c61df-f4c8-4213-bbe2-9c82dc852bd9"
		release   = "3b68894e-833f-4c7e-b317-89520afd9efe"
		artist    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	playPayload := `{"event":"media.play","Metadata":{"librarySectionType":"artist",` +
		`"guid":{"id":"mbid://` + recording + `"},` +
		`"parentGuid":{"id":"mbid://` + release + `"},` +
		`"grandparentGuid":{"id":"mbid://` + artist + `"}}}`
	keys := webhookIdentityKeysFromPayload(playPayload)
	if got, _ := keys.rank(recording); got != 2 {
		t.Errorf("recording rank = %d, want 2", got)
	}
	if got, _ := keys.rank(release); got != 1 {
		t.Errorf("release rank = %d, want 1", got)
	}
	if got, _ := keys.rank(artist); got != 0 {
		t.Errorf("artist rank = %d, want 0", got)
	}
	if _, ok := keys.rank("00000000-0000-0000-0000-000000000000"); ok {
		t.Error("an id the payload never carried must not rank")
	}

	// Jellyfin carries the same identities as ProviderIds without the mbid scheme.
	jellyfinPayload := `{"event":"PlaybackStart","Metadata":{"librarySectionType":"Audio",` +
		`"ProviderIds":{"MusicBrainzTrack":"` + recording + `",` +
		`"MusicBrainzAlbum":"` + release + `","MusicBrainzArtist":"` + artist + `"}}}`
	keys = webhookIdentityKeysFromPayload(jellyfinPayload)
	if got, _ := keys.rank(recording); got != 2 {
		t.Errorf("ProviderIds track rank = %d, want 2", got)
	}
	if got, _ := keys.rank(release); got != 1 {
		t.Errorf("ProviderIds album rank = %d, want 1", got)
	}
	if got, _ := keys.rank(artist); got != 0 {
		t.Errorf("ProviderIds artist rank = %d, want 0", got)
	}

	// An mbid in a field the structural reader does not model stays matchable, at the
	// lowest rank, so it can only win when nothing more specific matches.
	keys = webhookIdentityKeysFromPayload(`{"Metadata":{"SomeUnknownField":[{"id":"mbid://` + artist + `"}]}}`)
	if got, ok := keys.rank(artist); !ok || got != 0 {
		t.Errorf("fallback id rank = (%d, %v), want (0, true)", got, ok)
	}
}

func TestFindExactMatchIdentityRanking(t *testing.T) {
	const (
		recording = "b17c61df-f4c8-4213-bbe2-9c82dc852bd9"
		artist    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	now := time.Now()
	entries := []exactMatchEntry{
		{path: "/mnt/music/Artist/Album/01 - Coarse.flac", state: &PlaybackState{
			Path: "/mnt/music/Artist/Album/01 - Coarse.flac", ExternalID: artist,
			ExternalIDNamespace: "musicbrainz", OpenedAt: now,
		}},
		{path: "/mnt/music/Artist/Album/02 - Exact.flac", state: &PlaybackState{
			Path: "/mnt/music/Artist/Album/02 - Exact.flac", ExternalID: recording,
			ExternalIDNamespace: "musicbrainz", OpenedAt: now.Add(-time.Hour),
		}},
	}
	keys := webhookIdentityKeysFromPayload(`{"Metadata":{"guid":{"id":"mbid://` + recording + `"},` +
		`"grandparentGuid":{"id":"mbid://` + artist + `"}}}`)

	// The exact recording match beats the more recent artist match.
	path, _ := findExactMatch(exactMatchKeys{identities: keys}, entries)
	if path != "/mnt/music/Artist/Album/02 - Exact.flac" {
		t.Errorf("ranked match = %q, want the recording-registered session", path)
	}

	// The filename stays stronger than any identity.
	path, _ = findExactMatch(exactMatchKeys{identities: keys, basenames: []string{"01 - Coarse.flac"}}, entries)
	if path != "/mnt/music/Artist/Album/01 - Coarse.flac" {
		t.Errorf("basename match = %q, want the named file", path)
	}

	// A different namespace is not a MusicBrainz match, and an unknown id never matches.
	entries[1].state.ExternalIDNamespace = "asin"
	if _, state := findExactMatch(exactMatchKeys{identities: keys}, entries); state == nil {
		t.Fatal("the artist-registered session should still match at the lowest rank")
	} else if _, ns := state.GetExternalIdentity(); ns != "musicbrainz" {
		t.Error("a non-musicbrainz state was matched")
	}
	empty := webhookIdentityKeysFromPayload(`{"Metadata":{"guid":{"id":"mbid://00000000-0000-0000-0000-000000000000"}}}`)
	if path, _ := findExactMatch(exactMatchKeys{identities: empty}, entries); path != "" {
		t.Errorf("unknown id match = %q, want none", path)
	}
}
