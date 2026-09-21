package main

import (
	"reflect"
	"testing"
	"time"
)

func TestWebhookExternalIDs(t *testing.T) {
	payload := `{"Guid":[{"id":"mbid://B17C61DF-F4C8-4213-BBE2-9C82DC852BD9"},` +
		`{"id":"mbid://b17c61df-f4c8-4213-bbe2-9c82dc852bd9"},` +
		`{"id":"mbid://3b68894e-833f-4c7e-b317-89520afd9efe"},` +
		`{"id":"imdb://tt1234567"}]}`
	want := []string{
		"b17c61df-f4c8-4213-bbe2-9c82dc852bd9",
		"3b68894e-833f-4c7e-b317-89520afd9efe",
	}
	if got := webhookExternalIDs(payload); !reflect.DeepEqual(got, want) {
		t.Errorf("webhookExternalIDs = %v, want %v", got, want)
	}
	if got := webhookExternalIDs(`{"Guid":[{"id":"imdb://tt1234567"}]}`); got != nil {
		t.Errorf("webhookExternalIDs without an mbid = %v, want nil", got)
	}
}

func TestFindExactMatchExternalIdentity(t *testing.T) {
	const mbid = "b17c61df-f4c8-4213-bbe2-9c82dc852bd9"
	now := time.Now()
	entries := []exactMatchEntry{
		{path: "/mnt/music/Artist/Album/01 - Older.flac", state: &PlaybackState{
			Path: "/mnt/music/Artist/Album/01 - Older.flac", ExternalID: mbid,
			ExternalIDNamespace: "musicbrainz", OpenedAt: now.Add(-time.Hour),
		}},
		{path: "/mnt/music/Artist/Album/02 - Playing.flac", state: &PlaybackState{
			Path: "/mnt/music/Artist/Album/02 - Playing.flac", ExternalID: mbid,
			ExternalIDNamespace: "musicbrainz", OpenedAt: now,
		}},
	}

	// An album id is shared by every track, so the most recent session wins.
	path, state := findExactMatch(exactMatchKeys{externalIDs: []string{mbid}}, entries)
	if path != "/mnt/music/Artist/Album/02 - Playing.flac" || state == nil {
		t.Fatalf("identity match = (%q, %v), want the most recently opened track", path, state != nil)
	}

	// The filename stays stronger than any identity.
	path, _ = findExactMatch(exactMatchKeys{externalIDs: []string{mbid}, basenames: []string{"01 - Older.flac"}}, entries)
	if path != "/mnt/music/Artist/Album/01 - Older.flac" {
		t.Errorf("basename match = %q, want the named file", path)
	}

	// A different namespace is not a MusicBrainz match, and an unknown id never matches.
	entries[1].state.ExternalIDNamespace = "asin"
	if path, _ := findExactMatch(exactMatchKeys{externalIDs: []string{mbid}}, entries); path != "/mnt/music/Artist/Album/01 - Older.flac" {
		t.Errorf("namespace-mismatch match = %q, want only the musicbrainz state", path)
	}
	if path, _ := findExactMatch(exactMatchKeys{externalIDs: []string{"00000000-0000-0000-0000-000000000000"}}, entries); path != "" {
		t.Errorf("unknown id match = %q, want none", path)
	}
}
