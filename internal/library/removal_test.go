package library

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tiramisu/internal/metadb"
)

// writeRemovalStub plants a stub under a section root the way a completed add would
// have left it, and returns its physical path.
func writeRemovalStub(t *testing.T, root string, section Section, rel string) string {
	t.Helper()
	data, err := AudioStubBytes("http://stub.invalid/stream", 4096, "magnet:?xt=urn:btih:"+handlerAudioHash, "", "")
	if err != nil {
		t.Fatalf("AudioStubBytes: %v", err)
	}
	path := filepath.Join(root, string(section), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create stub directory: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func committedAudioRow(section Section, virtualPath string, fileIndex int) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:     string(section),
		VirtualPath: virtualPath,
		Hash:        handlerAudioHash,
		FileIndex:   fileIndex,
		Size:        4096,
		State:       metadb.AudioCommitted,
	}
}

func decodeAudioRemove(t *testing.T, recorder *httptest.ResponseRecorder) AudioRemoveResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var got AudioRemoveResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode remove response: %v", err)
	}
	return got
}

func TestHandlerRemoveAudio_removes_one_projection_and_keeps_the_torrent(t *testing.T) {
	const (
		musicPath = "Artist/Album/01 - Track.flac"
		bookPath  = "Author/Book/Part 01.m4b"
	)
	fixture := newHandlerAudioFixture(t, nil,
		committedAudioRow(SectionMusic, musicPath, 1),
		committedAudioRow(SectionAudiobooks, bookPath, 2),
	)
	musicStub := writeRemovalStub(t, fixture.root, SectionMusic, musicPath)
	bookStub := writeRemovalStub(t, fixture.root, SectionAudiobooks, bookPath)

	recorder := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "music", Path: musicPath})
	if recorder.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var got AudioRemoveResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode remove response: %v", err)
	}
	want := AudioRemoveResponse{Type: "music", Path: musicPath, Removed: true, TorrentReferenced: true}
	if got != want {
		t.Errorf("remove response = %+v, want %+v", got, want)
	}

	if _, err := os.Stat(musicStub); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removed stub still on disk: %v", err)
	}
	if _, err := os.Stat(bookStub); err != nil {
		t.Errorf("unrelated stub was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, string(SectionMusic))); err != nil {
		t.Errorf("section root must survive pruning: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, string(SectionMusic), "Artist")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty parent directories were not pruned: %v", err)
	}
	if _, ok := fixture.registry.projection(string(SectionMusic), musicPath); ok {
		t.Error("removed projection is still in the registry")
	}
	if _, ok := fixture.registry.projection(string(SectionAudiobooks), bookPath); !ok {
		t.Error("the sibling projection was dropped with the removed one")
	}
	unpublished := fixture.registry.unpublishedSnapshot()
	if len(unpublished) != 1 || unpublished[0].VirtualPath != musicPath {
		t.Errorf("unpublished = %+v, want only %q", unpublished, musicPath)
	}

	recorder = serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "audiobook", Path: bookPath})
	got = decodeAudioRemove(t, recorder)
	if got.Removed != true || got.TorrentReferenced {
		t.Errorf("last removal = %+v, want removed with torrent_referenced false", got)
	}
}

func TestHandlerRemoveAudio_absent_path_is_idempotent(t *testing.T) {
	const path = "Author/Book/Part 01.m4b"
	fixture := newHandlerAudioFixture(t, nil, committedAudioRow(SectionAudiobooks, path, 1))

	first := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "audiobook", Path: path})
	if first.Code != http.StatusOK {
		t.Fatalf("first remove status = %d, body %s", first.Code, first.Body.String())
	}
	second := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "audiobook", Path: path})
	want := AudioRemoveResponse{Type: "audiobook", Path: path, Removed: false, State: "absent"}
	if got := decodeAudioRemove(t, second); got != want {
		t.Errorf("repeated remove = %+v, want %+v", got, want)
	}
	// The stub never existed; the first removal still owns the registry row it claimed.
	if _, ok := fixture.registry.projection(string(SectionAudiobooks), path); ok {
		t.Error("repeated remove left the registry row behind")
	}
}

func TestHandlerRemoveAudio_rejects_blacklist_and_bad_paths(t *testing.T) {
	const path = "Artist/Album/01 - Track.flac"
	fixture := newHandlerAudioFixture(t, nil, committedAudioRow(SectionMusic, path, 1))
	stub := writeRemovalStub(t, fixture.root, SectionMusic, path)

	recorder := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "music", Path: path, Blacklist: true})
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("blacklist remove status = %d, want 400", recorder.Code)
	}
	if _, err := os.Stat(stub); err != nil {
		t.Errorf("blacklist rejection touched the stub: %v", err)
	}
	if _, ok := fixture.registry.projection(string(SectionMusic), path); !ok {
		t.Error("blacklist rejection touched the registry")
	}

	for _, bad := range []string{"", "../escape.flac", "/absolute.flac"} {
		recorder := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
			RemoveRequest{Type: "music", Path: bad})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("path %q status = %d, want 400", bad, recorder.Code)
		}
	}
}

func TestHandlerRemove_video_does_not_reach_the_audio_path(t *testing.T) {
	fixture := newHandlerAudioFixture(t, nil)

	recorder := serveHandlerJSON(t, fixture.handler.Remove, http.MethodPost, "/api/library/remove",
		RemoveRequest{Type: "movie", Path: "Whatever.mkv"})
	if recorder.Code == http.StatusOK {
		t.Errorf("legacy video remove unexpectedly succeeded: %s", recorder.Body.String())
	}
	for _, call := range fixture.registry.snapshot() {
		if call.method == "MarkAudioProjectionRemoving" {
			t.Errorf("video request reached the audio removal path: %+v", call)
		}
	}
}
