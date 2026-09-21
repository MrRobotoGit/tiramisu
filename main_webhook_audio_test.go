package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAudioMbid = "b17c61df-f4c8-4213-bbe2-9c82dc852bd9"

func postPlexWebhook(t *testing.T, body string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/plex/webhook", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handlePlexWebhook(httptest.NewRecorder(), request)
}

func playbackFlag(t *testing.T, ps *PlaybackState, read func() bool) bool {
	t.Helper()
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return read()
}

// A Plex music payload names an "artist" library section and often carries no
// Part/File to match on: the registered mbid is the only key that can bind the
// session, and the audio path must not fall back to name similarity.
func TestPlexWebhookMatchesAudioByIdentityOnly(t *testing.T) {
	playing := &PlaybackState{
		Path:       "/mnt/tiramisu/music/Prince - 20Ten (2010)/05. Act Of God_9ac7684d.flac",
		ExternalID: testAudioMbid, ExternalIDNamespace: "musicbrainz",
		OpenedAt: time.Now(),
	}
	decoy := &PlaybackState{
		Path:     "/mnt/tiramisu/music/Prince - 20Ten (2010)/03. Future Soul Song_9ac7684d.flac",
		OpenedAt: time.Now(),
	}
	playbackRegistry.Store(playing.Path, playing)
	playbackRegistry.Store(decoy.Path, decoy)
	t.Cleanup(func() {
		playbackRegistry.Delete(playing.Path)
		playbackRegistry.Delete(decoy.Path)
	})

	payload := `{"event":"media.play","Metadata":{"librarySectionType":"artist",` +
		`"title":"Act Of God","grandparentTitle":"Prince","year":2010,` +
		`"Guid":[{"id":"mbid://` + testAudioMbid + `"}]}}`
	postPlexWebhook(t, payload)

	if !playbackFlag(t, playing, func() bool { return playing.IsHealthy }) {
		t.Fatal("audio session was not confirmed by its mbid identity")
	}
	if playbackFlag(t, decoy, func() bool { return decoy.IsHealthy }) {
		t.Fatal("audio webhook confirmed a decoy state")
	}

	stop := `{"event":"media.stop","Metadata":{"librarySectionType":"artist",` +
		`"title":"Act Of God","grandparentTitle":"Prince",` +
		`"Guid":[{"id":"mbid://` + testAudioMbid + `"}]}}`
	postPlexWebhook(t, stop)

	if !playbackFlag(t, playing, func() bool { return playing.IsStopped }) {
		t.Fatal("audio stop with an identity match was not applied")
	}
	if playbackFlag(t, decoy, func() bool { return decoy.IsStopped }) {
		t.Fatal("audio stop applied to a decoy state")
	}
}

// Video payloads keep their own gate: a photo library event is still ignored.
func TestPlexWebhookIgnoresNonMediaSections(t *testing.T) {
	state := &PlaybackState{
		Path:       "/mnt/tiramisu/music/Album/01 - Track.flac",
		ExternalID: testAudioMbid, ExternalIDNamespace: "musicbrainz",
		OpenedAt: time.Now(),
	}
	playbackRegistry.Store(state.Path, state)
	t.Cleanup(func() { playbackRegistry.Delete(state.Path) })

	postPlexWebhook(t, `{"event":"media.play","Metadata":{"librarySectionType":"photo",`+
		`"Guid":[{"id":"mbid://`+testAudioMbid+`"}]}}`)

	if playbackFlag(t, state, func() bool { return state.IsHealthy }) {
		t.Fatal("a non-media section type confirmed a playback state")
	}
}

// Jellyfin reports audio differently: ItemType "Audio" and bare MusicBrainz uuids
// under ProviderIds, with event names of its own.
func TestPlexWebhookMatchesJellyfinAudioProviderIds(t *testing.T) {
	playing := &PlaybackState{
		Path:       "/mnt/tiramisu/music/Prince - 20Ten (2010)/05. Act Of God_9ac7684d.flac",
		ExternalID: testAudioMbid, ExternalIDNamespace: "musicbrainz",
		OpenedAt: time.Now(),
	}
	playbackRegistry.Store(playing.Path, playing)
	t.Cleanup(func() { playbackRegistry.Delete(playing.Path) })

	postPlexWebhook(t, `{"event":"PlaybackStart","Metadata":{"librarySectionType":"Audio",`+
		`"ProviderIds":{"MusicBrainzTrack":"`+testAudioMbid+`"}}}`)

	if !playbackFlag(t, playing, func() bool { return playing.IsHealthy }) {
		t.Fatal("Jellyfin audio session was not confirmed by its ProviderIds track id")
	}

	postPlexWebhook(t, `{"event":"PlaybackStop","Metadata":{"librarySectionType":"Audio",`+
		`"ProviderIds":{"MusicBrainzTrack":"`+testAudioMbid+`"}}}`)

	if !playbackFlag(t, playing, func() bool { return playing.IsStopped }) {
		t.Fatal("Jellyfin audio stop was not applied")
	}
}
