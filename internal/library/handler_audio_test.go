package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tiramisu/internal/metadb"
)

const handlerAudioHash = "0123456789abcdef0123456789abcdef01234567"

type handlerAudioEngineCall struct {
	method  string
	magnet  string
	title   string
	hash    string
	maxWait int
}

type handlerAudioEngineFake struct {
	mu sync.Mutex

	addHash  string
	addErr   error
	info     *TorrentStats
	infoErr  error
	torrents []TorrentStats
	listErr  error
	calls    []handlerAudioEngineCall
}

func (f *handlerAudioEngineFake) AddTorrent(_ context.Context, magnet, title string) (string, error) {
	f.record(handlerAudioEngineCall{method: "AddTorrent", magnet: magnet, title: title})
	return f.addHash, f.addErr
}

func (f *handlerAudioEngineFake) GetTorrentInfo(_ context.Context, hash string, maxWait int) (*TorrentStats, error) {
	f.record(handlerAudioEngineCall{method: "GetTorrentInfo", hash: hash, maxWait: maxWait})
	return f.info, f.infoErr
}

func (f *handlerAudioEngineFake) RemoveTorrent(_ context.Context, hash string) error {
	f.record(handlerAudioEngineCall{method: "RemoveTorrent", hash: hash})
	return nil
}

func (f *handlerAudioEngineFake) ListTorrents(context.Context) ([]TorrentStats, error) {
	f.record(handlerAudioEngineCall{method: "ListTorrents"})
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TorrentStats(nil), f.torrents...), f.listErr
}

func (f *handlerAudioEngineFake) record(call handlerAudioEngineCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *handlerAudioEngineFake) snapshot() []handlerAudioEngineCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]handlerAudioEngineCall(nil), f.calls...)
}

type handlerAudioRegistryCall struct {
	method  string
	section string
	prefix  string
	cursor  string
	limit   int
	rows    []metadb.AudioProjection
}

type handlerAudioRegistryFake struct {
	mu sync.Mutex

	rows        map[string]metadb.AudioProjection
	staged      map[string][]metadb.AudioProjection
	pageRows    []metadb.AudioProjection
	calls       []handlerAudioRegistryCall
	unpublished []AudioProjection
}

var _ AudioProjectionRegistry = (*handlerAudioRegistryFake)(nil)
var _ AudioRemovalRegistry = (*handlerAudioRegistryFake)(nil)
var _ AudioRegistry = (*handlerAudioRegistryFake)(nil)
var _ audioProjectionPager = (*handlerAudioRegistryFake)(nil)

func newHandlerAudioRegistryFake(initial ...metadb.AudioProjection) *handlerAudioRegistryFake {
	f := &handlerAudioRegistryFake{
		rows:   make(map[string]metadb.AudioProjection),
		staged: make(map[string][]metadb.AudioProjection),
	}
	for _, row := range initial {
		f.rows[handlerAudioProjectionKey(row.Section, row.VirtualPath)] = row
	}
	return f
}

func (f *handlerAudioRegistryFake) GetAudioProjection(section, virtualPath string) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "GetAudioProjection", section: section})
	row, ok := f.rows[handlerAudioProjectionKey(section, virtualPath)]
	if !ok {
		return nil, false, nil
	}
	copy := row
	return &copy, true, nil
}

func (f *handlerAudioRegistryFake) AudioProjectionBySource(hash string, fileIndex int) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "AudioProjectionBySource"})
	for _, row := range f.rows {
		if row.Hash == hash && row.FileIndex == fileIndex {
			copy := row
			return &copy, true, nil
		}
	}
	return nil, false, nil
}

func (f *handlerAudioRegistryFake) AudioProjectionByPortableKey(section, portableKey string) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "AudioProjectionByPortableKey", section: section})
	for _, row := range f.rows {
		if row.Section == section && row.PortablePathKey == portableKey {
			copy := row
			return &copy, true, nil
		}
	}
	return nil, false, nil
}

func (f *handlerAudioRegistryFake) StageAudioProjections(txnID string, rows []metadb.AudioProjection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := append([]metadb.AudioProjection(nil), rows...)
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "StageAudioProjections", rows: copy})
	f.staged[txnID] = copy
	return nil
}

func (f *handlerAudioRegistryFake) CommitAudioProjections(txnID string, _ int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := append([]metadb.AudioProjection(nil), f.staged[txnID]...)
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "CommitAudioProjections", rows: rows})
	for _, row := range rows {
		row.State = metadb.AudioCommitted
		row.TxnID = ""
		f.rows[handlerAudioProjectionKey(row.Section, row.VirtualPath)] = row
	}
	delete(f.staged, txnID)
	return len(rows), nil
}

func (f *handlerAudioRegistryFake) RollbackAudioProjections(txnID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.staged[txnID]
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "RollbackAudioProjections", rows: append([]metadb.AudioProjection(nil), rows...)})
	delete(f.staged, txnID)
	return len(rows), nil
}

func (f *handlerAudioRegistryFake) AudioHashReferenced(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "AudioHashReferenced"})
	return false, nil
}

func (f *handlerAudioRegistryFake) MarkAudioProjectionRemoving(section, virtualPath string, updatedAtNS int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "MarkAudioProjectionRemoving", section: section})
	key := handlerAudioProjectionKey(section, virtualPath)
	row, ok := f.rows[key]
	if !ok || row.State != metadb.AudioCommitted {
		return false, nil
	}
	row.State = metadb.AudioRemoving
	row.UpdatedAtNS = updatedAtNS
	f.rows[key] = row
	return true, nil
}

func (f *handlerAudioRegistryFake) DeleteAudioProjection(section, virtualPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "DeleteAudioProjection", section: section})
	delete(f.rows, handlerAudioProjectionKey(section, virtualPath))
	return nil
}

func (f *handlerAudioRegistryFake) AudioProjectionsByHash(hash string) ([]metadb.AudioProjection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{method: "AudioProjectionsByHash"})
	var rows []metadb.AudioProjection
	for _, row := range f.rows {
		if row.Hash == hash {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// recordUnpublished captures what the manager hands to the live namespace seam.
func (f *handlerAudioRegistryFake) recordUnpublished(p AudioProjection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpublished = append(f.unpublished, p)
}

func (f *handlerAudioRegistryFake) unpublishedSnapshot() []AudioProjection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AudioProjection(nil), f.unpublished...)
}

func (f *handlerAudioRegistryFake) projection(section, virtualPath string) (metadb.AudioProjection, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[handlerAudioProjectionKey(section, virtualPath)]
	return row, ok
}

func (f *handlerAudioRegistryFake) AudioProjectionPage(section, prefix, cursor string, limit int) ([]metadb.AudioProjection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, handlerAudioRegistryCall{
		method: "AudioProjectionPage", section: section, prefix: prefix, cursor: cursor, limit: limit,
	})
	rows := append([]metadb.AudioProjection(nil), f.pageRows...)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *handlerAudioRegistryFake) snapshot() []handlerAudioRegistryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]handlerAudioRegistryCall, len(f.calls))
	for i, call := range f.calls {
		calls[i] = call
		calls[i].rows = append([]metadb.AudioProjection(nil), call.rows...)
	}
	return calls
}

func handlerAudioProjectionKey(section, path string) string { return section + "\x00" + path }

type handlerAudioEpisodeRegistryFake struct {
	mu      sync.Mutex
	entries map[string]metadb.EpisodeEntry
}

func (f *handlerAudioEpisodeRegistryFake) UpsertEpisode(key string, entry metadb.EpisodeEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = make(map[string]metadb.EpisodeEntry)
	}
	f.entries[key] = entry
	return nil
}

func (f *handlerAudioEpisodeRegistryFake) GetEpisode(key string) (*metadb.EpisodeEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[key]
	if !ok {
		return nil, false, nil
	}
	copy := entry
	return &copy, true, nil
}

func (f *handlerAudioEpisodeRegistryFake) DeleteEpisode(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
	return nil
}

func (f *handlerAudioEpisodeRegistryFake) EpisodesByFilePath(path string) ([]metadb.EpisodeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var entries []metadb.EpisodeEntry
	for _, entry := range f.entries {
		if entry.FilePath == path {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

type handlerAudioFixture struct {
	root     string
	movies   string
	tv       string
	engine   *handlerAudioEngineFake
	registry *handlerAudioRegistryFake
	handler  *Handler
}

func newHandlerAudioFixture(t *testing.T, files []FileStat, initial ...metadb.AudioProjection) *handlerAudioFixture {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, string(SectionMovies))
	tv := filepath.Join(root, string(SectionTV))
	for _, dir := range []string{
		movies,
		tv,
		filepath.Join(root, string(SectionMusic)),
		filepath.Join(root, string(SectionAudiobooks)),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create fixture directory %q: %v", dir, err)
		}
	}
	engine := &handlerAudioEngineFake{
		addHash: handlerAudioHash,
		info: &TorrentStats{
			Hash:      handlerAudioHash,
			FileStats: append([]FileStat(nil), files...),
		},
	}
	registry := newHandlerAudioRegistryFake(initial...)
	manager := New(Config{
		MoviesDir:        movies,
		TVDir:            tv,
		GoStormURL:       "http://gostorm.invalid/base/",
		GoStorm:          engine,
		Registry:         &handlerAudioEpisodeRegistryFake{entries: make(map[string]metadb.EpisodeEntry)},
		AudioRegistry:    registry,
		AudioRoot:        root,
		AudioProjections: registry,
		AudioRemoval:     registry,
		UnpublishAudioPath: func(p AudioProjection) {
			registry.recordUnpublished(p)
		},
		Logger: log.New(io.Discard, "", 0),
	})
	return &handlerAudioFixture{
		root: root, movies: movies, tv: tv, engine: engine, registry: registry, handler: NewHandler(manager),
	}
}

func TestHandlerAddAudioDispatch_W1_W2(t *testing.T) {
	tests := []struct {
		name        string
		requestType string
		source      FileStat
		path        string
	}{
		{
			name:        "W1_music_reaches_audio_add_and_returns_201_audio_response",
			requestType: "music",
			source:      FileStat{ID: 7, Path: "Release/Disc 1/01 - Track.flac", Length: 34_567_890},
			path:        "Artist/Album/01 - Track_01234567.flac",
		},
		{
			name:        "W2_audiobook_reaches_audio_add_and_returns_201_audio_response",
			requestType: "audiobook",
			source:      FileStat{ID: 4, Path: "Book/Part 01.m4b", Length: 98_765_432},
			path:        "Author/Book/Part 01_01234567.m4b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandlerAudioFixture(t, []FileStat{tc.source})
			request := AddRequest{
				Type: tc.requestType, Hash: handlerAudioHash, Title: "Caller title",
				Files: []AudioFileRequest{{SourcePath: tc.source.Path, Path: tc.path}},
			}
			response := serveHandlerJSON(t, fixture.handler.Add, http.MethodPost, "/api/library/add", request)

			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusCreated, response.Body.Bytes())
			}
			assertJSONObjectKeys(t, response.Body.Bytes(), "already_present", "files", "hash", "title", "type")
			var got AudioAddResponse
			decodeHandlerResponse(t, response, &got)
			if got.AlreadyPresent {
				t.Error("AlreadyPresent = true, want false for a newly created projection")
			}
			if len(got.Files) != 1 {
				t.Fatalf("files = %#v, want exactly one", got.Files)
			}
			file := got.Files[0]
			if file.Path != tc.path || file.SourcePath != tc.source.Path || file.FileIndex != tc.source.ID ||
				file.Size != tc.source.Length || file.State != AudioProjectionCreated {
				t.Errorf("audio file = %#v, want the created projection for %+v", file, tc.source)
			}
			if _, err := time.Parse(time.RFC3339Nano, file.Mtime); err != nil {
				t.Errorf("mtime = %q, want RFC3339Nano: %v", file.Mtime, err)
			}
			var body struct {
				Files []map[string]json.RawMessage `json:"files"`
			}
			decodeHandlerResponse(t, response, &body)
			if len(body.Files) != 1 {
				t.Fatalf("files = %#v, want one audio file", body.Files)
			}
			assertRawMessageKeys(t, body.Files[0], "external_id", "external_id_ns", "file_index", "mtime", "path", "size", "source_path", "state")
			if !handlerAudioHasRegistryCall(fixture.registry.snapshot(), "StageAudioProjections") {
				t.Fatal("audio projection registry was not staged; request did not reach AddAudio")
			}
		})
	}
}

func TestHandlerAddVideoCompatibility_W3_W4_W5(t *testing.T) {
	tests := []struct {
		name        string
		requestType string
		wantType    string
		isTV        bool
	}{
		{name: "W3_W5_movie_keeps_legacy_status_and_body", requestType: "movie", wantType: "movie"},
		{name: "W4_W5_empty_default_keeps_legacy_movie_path", requestType: "", wantType: "movie"},
		{name: "W4_W5_movies_alias_keeps_legacy_movie_path", requestType: "movies", wantType: "movie"},
		{name: "W4_W5_film_alias_keeps_legacy_movie_path", requestType: "film", wantType: "movie"},
		{name: "W4_W5_tv_keeps_legacy_tv_path", requestType: "tv", wantType: "tv", isTV: true},
		{name: "W4_W5_show_alias_keeps_legacy_tv_path", requestType: "show", wantType: "tv", isTV: true},
		{name: "W4_W5_series_alias_keeps_legacy_tv_path", requestType: "series", wantType: "tv", isTV: true},
		{name: "W4_W5_episode_alias_keeps_legacy_tv_path", requestType: "episode", wantType: "tv", isTV: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := FileStat{ID: 3, Path: "Release/Feature.mkv", Length: 7_654_321}
			request := AddRequest{Type: tc.requestType, Hash: handlerAudioHash, Title: "Legacy title"}
			if tc.isTV {
				source.Path = "Release/Legacy.Title.S01E01.mkv"
				request.Season = 1
				request.Episode = 1
			}
			fixture := newHandlerAudioFixture(t, []FileStat{source})
			response := serveHandlerJSON(t, fixture.handler.Add, http.MethodPost, "/api/library/add", request)

			if response.Code != http.StatusCreated {
				t.Fatalf("legacy status = %d, want %d; body = %s", response.Code, http.StatusCreated, response.Body.Bytes())
			}
			assertJSONObjectKeys(t, response.Body.Bytes(), "already_present", "files", "hash", "title", "type")
			var got AddResponse
			decodeHandlerResponse(t, response, &got)
			if got.Hash != handlerAudioHash || got.Title != request.Title || got.Type != tc.wantType || got.AlreadyPresent {
				t.Fatalf("legacy response identity = %#v, want hash %q, title %q, canonical type %q, already_present false", got, handlerAudioHash, request.Title, tc.wantType)
			}
			if len(got.Files) != 1 || got.Files[0].FileIndex != source.ID || got.Files[0].Size != source.Length {
				t.Fatalf("legacy files = %#v, want one file with index %d and size %d", got.Files, source.ID, source.Length)
			}
			var body struct {
				Files []map[string]json.RawMessage `json:"files"`
			}
			decodeHandlerResponse(t, response, &body)
			fileKeys := []string{"file_index", "fuse_path", "path", "size"}
			if tc.isTV {
				fileKeys = append(fileKeys, "episode", "season")
			}
			assertRawMessageKeys(t, body.Files[0], fileKeys...)
			if calls := fixture.registry.snapshot(); len(calls) != 0 {
				t.Fatalf("audio registry calls for video request = %#v, want none", calls)
			}
			if methods := handlerAudioEngineMethods(fixture.engine.snapshot()); !reflect.DeepEqual(methods, []string{"AddTorrent", "GetTorrentInfo"}) {
				t.Fatalf("engine methods = %v, want legacy-only [AddTorrent GetTorrentInfo]", methods)
			}
			if strings.Contains(response.Body.String(), ErrRequestNotAudio.Error()) {
				t.Fatalf("internal dispatch sentinel leaked in successful video response: %s", response.Body.Bytes())
			}
		})
	}
}

func TestHandlerAddUnknownAndMalformed_W6_W8(t *testing.T) {
	t.Run("W6_unknown_type_keeps_the_exact_legacy_400", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, nil)
		response := serveHandlerJSON(t, fixture.handler.Add, http.MethodPost, "/api/library/add", AddRequest{
			Type: "documentary", Hash: handlerAudioHash, Title: "Unknown",
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.Bytes())
		}
		var got map[string]string
		decodeHandlerResponse(t, response, &got)
		want := map[string]string{"error": `unknown type "documentary": use movie or tv`}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("error body = %#v, want legacy body %#v", got, want)
		}
		if calls := fixture.registry.snapshot(); len(calls) != 0 {
			t.Fatalf("unknown type touched audio registry: %#v", calls)
		}
		if calls := fixture.engine.snapshot(); len(calls) != 0 {
			t.Fatalf("unknown type touched engine: %#v", calls)
		}
	})

	t.Run("W8_malformed_JSON_is_400_before_either_manager_path", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, nil)
		response := serveHandlerBody(fixture.handler.Add, http.MethodPost, "/api/library/add", []byte(`{"type":"music",`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.Bytes())
		}
		assertErrorObject(t, response.Body.Bytes())
		if calls := fixture.registry.snapshot(); len(calls) != 0 {
			t.Fatalf("malformed request touched audio manager collaborators: %#v", calls)
		}
		if calls := fixture.engine.snapshot(); len(calls) != 0 {
			t.Fatalf("malformed request touched legacy or audio manager collaborators: %#v", calls)
		}
	})
}

func TestHandlerAddAudioErrors_W7(t *testing.T) {
	validSource := FileStat{ID: 7, Path: "Release/Track.flac", Length: 123_456}
	validPath := "Artist/Album/Track_01234567.flac"
	conflict := metadb.AudioProjection{
		Section:         string(SectionMusic),
		VirtualPath:     validPath,
		PortablePathKey: PortablePathKey(validPath),
		Hash:            strings.Repeat("f", 40),
		FileIndex:       99,
		SourcePath:      "Other/Track.flac",
		Size:            999,
		State:           metadb.AudioCommitted,
	}
	tests := []struct {
		name              string
		wantStatus        int
		wantErrorContains string
		files             []FileStat
		initial           []metadb.AudioProjection
		request           AudioFileRequest
	}{
		{
			name: "W7_path_conflict_keeps_409", wantStatus: http.StatusConflict, wantErrorContains: metadb.ErrAudioPathConflict.Error(),
			files: []FileStat{validSource}, initial: []metadb.AudioProjection{conflict},
			request: AudioFileRequest{SourcePath: validSource.Path, Path: validPath},
		},
		{
			name: "W7_unresolvable_source_keeps_422", wantStatus: http.StatusUnprocessableEntity, wantErrorContains: ErrSourceNotFound.Error(),
			files:   []FileStat{validSource},
			request: AudioFileRequest{SourcePath: "Release/Missing.flac", Path: validPath},
		},
		{
			name: "W7_invalid_projection_path_keeps_400", wantStatus: http.StatusBadRequest, wantErrorContains: ErrPathInvalid.Error(),
			files:   []FileStat{validSource},
			request: AudioFileRequest{SourcePath: validSource.Path, Path: "../Track_01234567.flac"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandlerAudioFixture(t, tc.files, tc.initial...)
			response := serveHandlerJSON(t, fixture.handler.Add, http.MethodPost, "/api/library/add", AddRequest{
				Type: "music", Hash: handlerAudioHash, Title: "Audio failure", Files: []AudioFileRequest{tc.request},
			})
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tc.wantStatus, response.Body.Bytes())
			}
			message := decodeHandlerError(t, response.Body.Bytes())
			if !strings.Contains(message, tc.wantErrorContains) {
				t.Fatalf("error = %q, want audio failure containing %q", message, tc.wantErrorContains)
			}
			if handlerAudioHasRegistryCall(fixture.registry.snapshot(), "StageAudioProjections") {
				t.Fatalf("failed audio request staged a projection: %#v", fixture.registry.snapshot())
			}
		})
	}
}

func TestHandlerInspect_W9_W10_W11_W12(t *testing.T) {
	t.Run("W9_valid_request_returns_200_and_empty_files_as_array", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, []FileStat{})
		response := serveHandlerJSON(t, fixture.handler.Inspect, http.MethodPost, "/api/library/inspect", InspectRequest{
			Hash: handlerAudioHash, Title: "Empty torrent",
		})
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.Bytes())
		}
		assertJSONObjectKeys(t, response.Body.Bytes(), "files", "hash")
		var got InspectResponse
		decodeHandlerResponse(t, response, &got)
		if got.Hash != handlerAudioHash {
			t.Errorf("hash = %q, want %q", got.Hash, handlerAudioHash)
		}
		if got.Files == nil || len(got.Files) != 0 {
			t.Fatalf("files = %#v, want a decoded non-nil empty array", got.Files)
		}
	})

	t.Run("W10_GET_is_405_like_the_other_POST_routes", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, nil)
		response := serveHandlerBody(fixture.handler.Inspect, http.MethodGet, "/api/library/inspect", nil)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusMethodNotAllowed, response.Body.Bytes())
		}
		var got map[string]string
		decodeHandlerResponse(t, response, &got)
		if !reflect.DeepEqual(got, map[string]string{"error": "POST only"}) {
			t.Fatalf("body = %#v, want the established POST-route error", got)
		}
		assertHandlerAudioManagerUntouched(t, fixture)
	})

	t.Run("W11_malformed_body_is_400_without_calling_the_manager", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, nil)
		response := serveHandlerBody(fixture.handler.Inspect, http.MethodPost, "/api/library/inspect", []byte(`{"hash":`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.Bytes())
		}
		assertErrorObject(t, response.Body.Bytes())
		assertHandlerAudioManagerUntouched(t, fixture)
	})

	t.Run("W12_metadata_not_ready_keeps_504", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, nil)
		fixture.engine.info = nil
		fixture.engine.infoErr = errors.New("metadata deadline")
		response := serveHandlerJSON(t, fixture.handler.Inspect, http.MethodPost, "/api/library/inspect", InspectRequest{
			Hash: handlerAudioHash, Title: "Slow torrent", MetadataWait: 3,
		})
		if response.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusGatewayTimeout, response.Body.Bytes())
		}
		assertErrorObject(t, response.Body.Bytes())
	})
}

func TestHandlerListAudioShape_W13(t *testing.T) {
	tests := []struct {
		name        string
		requestType string
		section     string
		path        string
	}{
		{
			name: "W13_music_returns_an_audio_object_not_the_legacy_array", requestType: "music",
			section: string(SectionMusic), path: "Artist/Album/Track_01234567.flac",
		},
		{
			name: "W13_audiobook_returns_an_audio_object_not_the_legacy_array", requestType: "audiobook",
			section: string(SectionAudiobooks), path: "Author/Book/Part_01234567.m4b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandlerAudioFixture(t, nil)
			row := metadb.AudioProjection{
				Section: tc.section, VirtualPath: tc.path, Hash: handlerAudioHash,
				SourcePath: "Release/source", FileIndex: 6, Size: 654_321,
				MtimeNS: 1_700_000_000_123, State: metadb.AudioCommitted,
			}
			fixture.registry.pageRows = []metadb.AudioProjection{row}
			response := serveHandlerBody(fixture.handler.List, http.MethodGet, "/api/library/list?type="+url.QueryEscape(tc.requestType), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.Bytes())
			}
			assertJSONObjectKeys(t, response.Body.Bytes(), "items", "next_cursor")
			var got AudioListResponse
			decodeHandlerResponse(t, response, &got)
			want := AudioListResponse{
				Items: []AudioListItem{{
					Type: tc.requestType, Path: row.VirtualPath, Hash: row.Hash,
					SourcePath: row.SourcePath, FileIndex: row.FileIndex, Size: row.Size,
					MtimeNS: row.MtimeNS, State: string(row.State),
				}},
				NextCursor: "",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("audio list = %#v, want %#v", got, want)
			}
			calls := handlerAudioRegistryCallsFor(fixture.registry.snapshot(), "AudioProjectionPage")
			if len(calls) != 1 || calls[0].section != tc.section {
				t.Fatalf("projection-page calls = %#v, want one for section %q", calls, tc.section)
			}
		})
	}
}

func TestHandlerListLegacyCompatibility_W14_W15(t *testing.T) {
	t.Run("W14_gaps_keeps_legacy_array_and_total_count_header", func(t *testing.T) {
		want := []Gap{
			{EpisodeKey: "show_s01e01", Show: "Show", Season: 1, Path: "/tv/show/e1.mkv", DeadHash: "dead-one", RemovedAt: 11},
			{EpisodeKey: "show_s01e02", Show: "Show", Season: 1, Path: "/tv/show/e2.mkv", DeadHash: "dead-two", RemovedAt: 12},
		}
		registry := newHandlerAudioRegistryFake()
		handler := NewHandler(New(Config{
			Gaps:             func() ([]Gap, error) { return append([]Gap(nil), want...), nil },
			AudioProjections: registry,
			Logger:           log.New(io.Discard, "", 0),
		}))
		response := serveHandlerBody(handler.List, http.MethodGet, "/api/library/list?type=gaps", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.Bytes())
		}
		if got := response.Header().Get("X-Total-Count"); got != "2" {
			t.Fatalf("X-Total-Count = %q, want %q", got, "2")
		}
		var got []Gap
		decodeHandlerResponse(t, response, &got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("gaps = %#v, want %#v", got, want)
		}
		if calls := registry.snapshot(); len(calls) != 0 {
			t.Fatalf("gaps request touched audio listing: %#v", calls)
		}
	})

	for _, tc := range []struct {
		name        string
		requestType string
		dir         func(*handlerAudioFixture) string
		filename    string
	}{
		{
			name: "W15_movie_keeps_the_legacy_array_shape", requestType: "movie",
			dir: func(f *handlerAudioFixture) string { return f.movies }, filename: "Legacy_01234567.mkv",
		},
		{
			name: "W15_tv_keeps_the_legacy_array_shape", requestType: "tv",
			dir: func(f *handlerAudioFixture) string { return f.tv }, filename: "Legacy_S01E02_01234567.mkv",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandlerAudioFixture(t, nil)
			path := filepath.Join(tc.dir(fixture), tc.filename)
			if err := WriteStub(path, "http://gostorm.invalid/stream?link="+handlerAudioHash+"&index=9&play", 99_999, "magnet:?xt=urn:btih:"+handlerAudioHash, "tt1234567"); err != nil {
				t.Fatalf("create legacy stub: %v", err)
			}
			response := serveHandlerBody(fixture.handler.List, http.MethodGet, "/api/library/list?type="+tc.requestType, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.Bytes())
			}
			var generic interface{}
			decodeHandlerResponse(t, response, &generic)
			if _, ok := generic.([]interface{}); !ok {
				t.Fatalf("top-level JSON = %T (%#v), want legacy array", generic, generic)
			}
			var got []Item
			decodeHandlerResponse(t, response, &got)
			if len(got) != 1 || got[0].Path != path || got[0].Hash != handlerAudioHash || got[0].FileIndex != 9 || got[0].Size != 99_999 {
				t.Fatalf("legacy items = %#v, want the fixture stub", got)
			}
			if calls := fixture.registry.snapshot(); len(calls) != 0 {
				t.Fatalf("legacy list touched audio registry: %#v", calls)
			}
		})
	}
}

func TestHandlerListAudioQueryForwarding_W16(t *testing.T) {
	tests := []struct {
		name        string
		requestType string
		limit       string
		wantSection string
		wantLimit   int
	}{
		{
			name: "W16_prefix_limit_and_cursor_reach_ListAudio_unchanged", requestType: "music", limit: "7",
			wantSection: string(SectionMusic), wantLimit: 8,
		},
		{
			name: "W16_non_numeric_limit_uses_the_bounded_default", requestType: "audiobook", limit: "not-a-number",
			wantSection: string(SectionAudiobooks), wantLimit: metadb.DefaultAudioProjectionPageLimit + 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHandlerAudioFixture(t, nil)
			prefix := "Ärtist/% Live/Disc 01/"
			cursor := "Ärtist/% Live/Disc 01/07 + Track_01234567.flac"
			query := url.Values{
				"type":   []string{tc.requestType},
				"prefix": []string{prefix},
				"limit":  []string{tc.limit},
				"cursor": []string{cursor},
			}
			response := serveHandlerBody(fixture.handler.List, http.MethodGet, "/api/library/list?"+query.Encode(), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.Bytes())
			}
			var got AudioListResponse
			decodeHandlerResponse(t, response, &got)
			calls := handlerAudioRegistryCallsFor(fixture.registry.snapshot(), "AudioProjectionPage")
			if len(calls) != 1 {
				t.Fatalf("projection-page calls = %#v, want exactly one", calls)
			}
			call := calls[0]
			if call.section != tc.wantSection || call.prefix != prefix || call.cursor != cursor || call.limit != tc.wantLimit {
				t.Fatalf("projection page request = %#v, want section %q, prefix %q, cursor %q, limit %d", call, tc.wantSection, prefix, cursor, tc.wantLimit)
			}
			if call.limit > metadb.MaxAudioProjectionPageLimit+1 {
				t.Fatalf("projection page limit = %d, exceeds bounded maximum %d", call.limit, metadb.MaxAudioProjectionPageLimit+1)
			}
		})
	}
}

func serveHandlerJSON(t *testing.T, handler http.HandlerFunc, method, target string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return serveHandlerBody(handler, method, target, encoded)
}

func serveHandlerBody(handler http.HandlerFunc, method, target string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func decodeHandlerResponse(t *testing.T, response *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.Bytes(), err)
	}
}

func assertJSONObjectKeys(t *testing.T, body []byte, want ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode object body %q: %v", body, err)
	}
	assertRawMessageKeys(t, object, want...)
}

func assertRawMessageKeys(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}

func assertErrorObject(t *testing.T, body []byte) {
	t.Helper()
	decodeHandlerError(t, body)
}

func decodeHandlerError(t *testing.T, body []byte) string {
	t.Helper()
	assertJSONObjectKeys(t, body, "error")
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	if got["error"] == "" {
		t.Fatalf("error body = %#v, want a non-empty error", got)
	}
	return got["error"]
}

func assertHandlerAudioManagerUntouched(t *testing.T, fixture *handlerAudioFixture) {
	t.Helper()
	if calls := fixture.engine.snapshot(); len(calls) != 0 {
		t.Fatalf("manager engine calls = %#v, want none", calls)
	}
	if calls := fixture.registry.snapshot(); len(calls) != 0 {
		t.Fatalf("manager audio registry calls = %#v, want none", calls)
	}
}

func handlerAudioEngineMethods(calls []handlerAudioEngineCall) []string {
	methods := make([]string, len(calls))
	for i, call := range calls {
		methods[i] = call.method
	}
	return methods
}

func handlerAudioHasRegistryCall(calls []handlerAudioRegistryCall, method string) bool {
	return len(handlerAudioRegistryCallsFor(calls, method)) > 0
}

func handlerAudioRegistryCallsFor(calls []handlerAudioRegistryCall, method string) []handlerAudioRegistryCall {
	var matching []handlerAudioRegistryCall
	for _, call := range calls {
		if call.method == method {
			matching = append(matching, call)
		}
	}
	return matching
}

// M8: an all-present replay is a 200 with already_present true, and the file carries the
// committed state and mtime instead of looking like a new creation.
func TestHandlerAddAudioReplayIs200AlreadyPresent_M8(t *testing.T) {
	files := []FileStat{{ID: 7, Path: "Release/Disc 1/01 - Track.flac", Length: 34_567_890}}
	path := "Artist/Album/01 - Track_01234567.flac"
	initial := metadb.AudioProjection{
		Section: string(SectionMusic), VirtualPath: path, PortablePathKey: PortablePathKey(path),
		Hash: handlerAudioHash, FileIndex: files[0].ID, SourcePath: files[0].Path,
		Size: files[0].Length, MtimeNS: 1_700_000_000_000_000_000, Title: "T",
		Magnet: "magnet:?existing", State: metadb.AudioCommitted,
	}
	fixture := newHandlerAudioFixture(t, files, initial)
	request := AddRequest{
		Type: "music", Hash: handlerAudioHash, Title: "T",
		Files: []AudioFileRequest{{SourcePath: files[0].Path, Path: path}},
	}
	response := serveHandlerJSON(t, fixture.handler.Add, http.MethodPost, "/api/library/add", request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.Bytes())
	}
	var got AudioAddResponse
	decodeHandlerResponse(t, response, &got)
	if !got.AlreadyPresent {
		t.Error("already_present = false, want true for an all-present replay")
	}
	if len(got.Files) != 1 || got.Files[0].State != AudioProjectionPresent {
		t.Fatalf("files = %#v, want one present projection", got.Files)
	}
	wantMtime := time.Unix(0, initial.MtimeNS).UTC().Format(time.RFC3339Nano)
	if got.Files[0].Mtime != wantMtime {
		t.Errorf("mtime = %q, want the committed %q", got.Files[0].Mtime, wantMtime)
	}
	if calls := fixture.registry.snapshot(); handlerAudioHasRegistryCall(calls, "StageAudioProjections") {
		t.Errorf("replay staged rows: %#v", calls)
	}
}

// M9: the cap bounds the request, not just its allocation. A valid JSON value followed
// by excess must be rejected without reaching the registry.
func TestHandlerAddRejectsOversizedBody_M9(t *testing.T) {
	files := []FileStat{{ID: 7, Path: "Release/01.flac", Length: 4 << 20}}
	fixture := newHandlerAudioFixture(t, files)
	request := AddRequest{
		Type: "music", Hash: handlerAudioHash, Title: "T",
		Files: []AudioFileRequest{{SourcePath: files[0].Path, Path: "Artist/Album/01_01234567.flac"}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, bytes.Repeat([]byte(" "), maxBodyBytes)...)

	response := serveHandlerBody(fixture.handler.Add, http.MethodPost, "/api/library/add", body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.Bytes())
	}
	if !strings.Contains(response.Body.String(), "exceeds") {
		t.Errorf("body = %s, want the size rejection", response.Body.Bytes())
	}
	if calls := fixture.registry.snapshot(); len(calls) != 0 {
		t.Errorf("registry calls = %#v, want none for a rejected body", calls)
	}
}

// M9: audio requests reject fields their contract does not define; the legacy video
// decoder keeps accepting them, because its callers predate this endpoint.
func TestHandlerAddUnknownFields_M9(t *testing.T) {
	files := []FileStat{{ID: 7, Path: "Release/01.flac", Length: 4 << 20}}
	t.Run("M9a_audio_rejects_an_unknown_field", func(t *testing.T) {
		fixture := newHandlerAudioFixture(t, files)
		body := []byte(fmt.Sprintf(
			`{"type":"music","title":"T","hash":%q,"files":[{"source_path":%q,"path":"Artist/Album/01_01234567.flac"}],"filez":123}`,
			handlerAudioHash, files[0].Path))
		response := serveHandlerBody(fixture.handler.Add, http.MethodPost, "/api/library/add", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.Bytes())
		}
		if calls := fixture.registry.snapshot(); len(calls) != 0 {
			t.Errorf("registry calls = %#v, want none for a rejected request", calls)
		}
	})

	t.Run("M9b_video_keeps_accepting_an_unknown_field", func(t *testing.T) {
		source := FileStat{ID: 3, Path: "Release/Feature.mkv", Length: 7_654_321}
		fixture := newHandlerAudioFixture(t, []FileStat{source})
		body := []byte(fmt.Sprintf(`{"type":"movie","title":"Legacy","hash":%q,"vendor_field":1}`, handlerAudioHash))
		response := serveHandlerBody(fixture.handler.Add, http.MethodPost, "/api/library/add", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("legacy status = %d, want %d; body = %s", response.Code, http.StatusCreated, response.Body.Bytes())
		}
	})
}
