package library

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"tiramisu/internal/metadb"
)

const (
	inspectHexHash    = "0123456789abcdef0123456789abcdef01234567"
	inspectBase32Hash = "abcdefghijklmnopqrstuvwxyz234567"
)

type inspectEngineCall struct {
	Method  string
	Magnet  string
	Title   string
	Hash    string
	MaxWait int
}

type inspectGoStorm struct {
	mu sync.Mutex

	addHash   string
	addErr    error
	info      *TorrentStats
	infoErr   error
	torrents  []TorrentStats
	listErr   error
	removeErr error
	getInfo   func(context.Context, string, int) (*TorrentStats, error)

	calls []inspectEngineCall
}

func (f *inspectGoStorm) AddTorrent(_ context.Context, magnet, title string) (string, error) {
	f.record(inspectEngineCall{Method: "AddTorrent", Magnet: magnet, Title: title})
	return f.addHash, f.addErr
}

func (f *inspectGoStorm) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	f.record(inspectEngineCall{Method: "GetTorrentInfo", Hash: hash, MaxWait: maxWait})
	if f.getInfo != nil {
		return f.getInfo(ctx, hash, maxWait)
	}
	return f.info, f.infoErr
}

func (f *inspectGoStorm) RemoveTorrent(_ context.Context, hash string) error {
	f.record(inspectEngineCall{Method: "RemoveTorrent", Hash: hash})
	return f.removeErr
}

func (f *inspectGoStorm) ListTorrents(context.Context) ([]TorrentStats, error) {
	f.record(inspectEngineCall{Method: "ListTorrents"})
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TorrentStats(nil), f.torrents...), f.listErr
}

func (f *inspectGoStorm) record(call inspectEngineCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *inspectGoStorm) snapshot() []inspectEngineCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inspectEngineCall(nil), f.calls...)
}

type inspectRegistrySpy struct {
	mu    sync.Mutex
	calls []string
}

func (s *inspectRegistrySpy) UpsertEpisode(string, metadb.EpisodeEntry) error {
	s.record("UpsertEpisode")
	return errors.New("inspect must not upsert an episode")
}

func (s *inspectRegistrySpy) GetEpisode(string) (*metadb.EpisodeEntry, bool, error) {
	s.record("GetEpisode")
	return nil, false, errors.New("inspect must not read an episode")
}

func (s *inspectRegistrySpy) DeleteEpisode(string) error {
	s.record("DeleteEpisode")
	return errors.New("inspect must not delete an episode")
}

func (s *inspectRegistrySpy) EpisodesByFilePath(string) ([]metadb.EpisodeEntry, error) {
	s.record("EpisodesByFilePath")
	return nil, errors.New("inspect must not query episodes")
}

func (s *inspectRegistrySpy) AudioHashReferenced(string) (bool, error) {
	s.record("AudioHashReferenced")
	return false, errors.New("inspect must not query audio projections")
}

func (s *inspectRegistrySpy) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
}

func (s *inspectRegistrySpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestInspect_IdentityAndValidation(t *testing.T) {
	t.Run("I1_valid_40_hex_hash_builds_the_magnet", func(t *testing.T) {
		engine := successfulInspectEngine(inspectHexHash, nil)
		manager := newInspectManager(engine, nil)
		title := "An Album & Its Artist"

		response, err := manager.Inspect(context.Background(), InspectRequest{
			Hash:  inspectHexHash,
			Title: title,
		})
		if err != nil {
			t.Fatalf("Inspect() error = %v, want nil", err)
		}
		if response.Hash != inspectHexHash {
			t.Errorf("response Hash = %q, want %q", response.Hash, inspectHexHash)
		}
		calls := inspectCallsFor(engine, "AddTorrent")
		if len(calls) != 1 {
			t.Fatalf("AddTorrent call count = %d, want 1; calls = %+v", len(calls), engine.snapshot())
		}
		wantMagnet := BuildMagnet(inspectHexHash, title, DefaultTrackers())
		if calls[0].Magnet != wantMagnet || calls[0].Title != title {
			t.Errorf("AddTorrent args = (magnet %q, title %q), want (%q, %q)", calls[0].Magnet, calls[0].Title, wantMagnet, title)
		}
	})

	t.Run("I2_magnet_uses_the_hash_reported_by_GoStorm", func(t *testing.T) {
		magnet := "magnet:?xt=urn:btih:" + inspectBase32Hash + "&dn=Base32"
		engine := successfulInspectEngine(inspectHexHash, nil)
		manager := newInspectManager(engine, nil)

		response, err := manager.Inspect(context.Background(), InspectRequest{
			Magnet: magnet,
			Title:  "Base32",
		})
		if err != nil {
			t.Fatalf("Inspect() error = %v, want nil", err)
		}
		if response.Hash != inspectHexHash {
			t.Errorf("response Hash = %q, want GoStorm-reported hex %q rather than caller hash %q", response.Hash, inspectHexHash, inspectBase32Hash)
		}
		calls := inspectCallsFor(engine, "AddTorrent")
		if len(calls) != 1 || calls[0].Magnet != magnet {
			t.Errorf("AddTorrent calls = %+v, want one call with caller magnet %q", calls, magnet)
		}
	})

	t.Run("I3_title_is_required_and_does_not_shape_a_successful_response", func(t *testing.T) {
		missingTitleEngine := successfulInspectEngine(inspectHexHash, nil)
		response, err := newInspectManager(missingTitleEngine, nil).Inspect(context.Background(), InspectRequest{Hash: inspectHexHash})
		assertInspectStatus(t, response, err, http.StatusBadRequest)
		if calls := missingTitleEngine.snapshot(); len(calls) != 0 {
			t.Errorf("GoStorm calls for missing title = %+v, want none", calls)
		}

		title := "Needle Title"
		files := []FileStat{
			{ID: 8, Path: "unrelated/first.cue", Length: 80},
			{ID: 3, Path: "other/second.flac", Length: 300},
		}
		engine := successfulInspectEngine(inspectHexHash, files)
		got, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{Hash: inspectHexHash, Title: title})
		if err != nil {
			t.Fatalf("Inspect() error = %v, want nil", err)
		}
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want title-independent %+v", got.Files, want)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal(response): %v", err)
		}
		if strings.Contains(string(encoded), title) {
			t.Errorf("response JSON %s contains request title %q", encoded, title)
		}
	})

	t.Run("I4_hash_or_magnet_is_required", func(t *testing.T) {
		engine := successfulInspectEngine(inspectHexHash, nil)
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{Title: "No identity"})
		assertInspectStatus(t, response, err, http.StatusBadRequest)
		if calls := engine.snapshot(); len(calls) != 0 {
			t.Errorf("GoStorm calls = %+v, want none", calls)
		}
	})

	t.Run("I5_malformed_hash_is_rejected_without_any_GoStorm_call", func(t *testing.T) {
		engine := successfulInspectEngine(inspectHexHash, nil)
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  "not-a-hex-or-base32-info-hash",
			Title: "Malformed",
		})
		assertInspectStatus(t, response, err, http.StatusBadRequest)
		if calls := engine.snapshot(); len(calls) != 0 {
			t.Errorf("GoStorm calls = %+v, want none", calls)
		}
	})

	t.Run("I5_valid_32_character_base32_hash_is_accepted", func(t *testing.T) {
		engine := successfulInspectEngine(inspectHexHash, nil)
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  inspectBase32Hash,
			Title: "Base32 hash",
		})
		if err != nil {
			t.Fatalf("Inspect() error = %v, want nil", err)
		}
		if response.Hash != inspectHexHash {
			t.Errorf("response Hash = %q, want GoStorm-reported %q", response.Hash, inspectHexHash)
		}
		if calls := inspectCallsFor(engine, "AddTorrent"); len(calls) != 1 {
			t.Errorf("AddTorrent call count = %d, want 1; calls = %+v", len(calls), engine.snapshot())
		}
	})

	t.Run("I6_malformed_GoStorm_hash_is_502_and_drops_the_requested_hash", func(t *testing.T) {
		engine := &inspectGoStorm{addHash: "malformed-engine-hash"}
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  inspectHexHash,
			Title: "Bad engine response",
		})
		assertInspectStatus(t, response, err, http.StatusBadGateway)
		removeCalls := inspectCallsFor(engine, "RemoveTorrent")
		if len(removeCalls) != 1 || removeCalls[0].Hash != inspectHexHash {
			t.Errorf("RemoveTorrent calls = %+v, want requested hash %q removed once", removeCalls, inspectHexHash)
		}
		if calls := inspectCallsFor(engine, "GetTorrentInfo"); len(calls) != 0 {
			t.Errorf("GetTorrentInfo calls = %+v, want none after malformed engine hash", calls)
		}
	})
}

func TestInspect_MetadataWaitAndCleanup(t *testing.T) {
	for _, tt := range []struct {
		name      string
		requested int
		want      int
	}{
		{name: "I7_zero_wait_uses_the_default", requested: 0, want: defaultMetadataWait},
		{name: "I7_negative_wait_uses_the_default", requested: -9, want: defaultMetadataWait},
		{name: "I7_in_range_wait_is_unchanged", requested: 17, want: 17},
		{name: "I7_wait_above_the_maximum_is_capped", requested: maxMetadataWait + 1, want: maxMetadataWait},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := successfulInspectEngine(inspectHexHash, nil)
			response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
				Hash:         inspectHexHash,
				Title:        "Wait bounds",
				MetadataWait: tt.requested,
			})
			if err != nil || response == nil {
				t.Fatalf("Inspect() = (%+v, %v), want non-nil response and nil error", response, err)
			}
			calls := inspectCallsFor(engine, "GetTorrentInfo")
			if len(calls) != 1 || calls[0].MaxWait != tt.want {
				t.Errorf("GetTorrentInfo calls = %+v, want one call with maxWait %d", calls, tt.want)
			}
		})
	}

	t.Run("I8_metadata_not_ready_is_504_not_an_empty_success", func(t *testing.T) {
		engine := &inspectGoStorm{addHash: inspectHexHash, info: nil, infoErr: nil}
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  inspectHexHash,
			Title: "Not ready",
		})
		assertInspectStatus(t, response, err, http.StatusGatewayTimeout)
		if calls := inspectCallsFor(engine, "GetTorrentInfo"); len(calls) != 1 {
			t.Errorf("GetTorrentInfo call count = %d, want 1; calls = %+v", len(calls), engine.snapshot())
		}
	})

	t.Run("I9_metadata_failure_removes_a_torrent_inspect_added", func(t *testing.T) {
		engine := &inspectGoStorm{addHash: inspectHexHash, info: nil}
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  inspectHexHash,
			Title: "Inspect-owned",
		})
		assertInspectStatus(t, response, err, http.StatusGatewayTimeout)
		if calls := inspectCallsFor(engine, "ListTorrents"); len(calls) != 1 {
			t.Errorf("ListTorrents call count = %d, want 1 to establish prior ownership", len(calls))
		}
		if calls := inspectCallsFor(engine, "AddTorrent"); len(calls) != 1 {
			t.Errorf("AddTorrent call count = %d, want 1", len(calls))
		}
		removeCalls := inspectCallsFor(engine, "RemoveTorrent")
		if len(removeCalls) != 1 || removeCalls[0].Hash != inspectHexHash {
			t.Errorf("RemoveTorrent calls = %+v, want inspect-added hash %q removed once", removeCalls, inspectHexHash)
		}
	})

	t.Run("I10_metadata_failure_never_removes_a_preexisting_torrent", func(t *testing.T) {
		engine := &inspectGoStorm{
			addHash:  inspectHexHash,
			info:     nil,
			torrents: []TorrentStats{{Hash: inspectHexHash}},
		}
		response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
			Hash:  inspectHexHash,
			Title: "Already present",
		})
		assertInspectStatus(t, response, err, http.StatusGatewayTimeout)
		if calls := inspectCallsFor(engine, "ListTorrents"); len(calls) != 1 {
			t.Errorf("ListTorrents call count = %d, want 1 to observe the preexisting torrent", len(calls))
		}
		if calls := inspectCallsFor(engine, "GetTorrentInfo"); len(calls) != 1 {
			t.Errorf("GetTorrentInfo call count = %d, want 1", len(calls))
		}
		if calls := inspectCallsFor(engine, "RemoveTorrent"); len(calls) != 0 {
			t.Errorf("RemoveTorrent calls = %+v, want none for a preexisting torrent", calls)
		}
	})

	t.Run("I11_context_cancellation_interrupts_the_metadata_wait", func(t *testing.T) {
		entered := make(chan struct{})
		var enteredOnce sync.Once
		engine := &inspectGoStorm{addHash: inspectHexHash}
		engine.getInfo = func(ctx context.Context, _ string, _ int) (*TorrentStats, error) {
			enteredOnce.Do(func() { close(entered) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		manager := newInspectManager(engine, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		type result struct {
			response *InspectResponse
			err      error
		}
		done := make(chan result, 1)
		go func() {
			response, err := manager.Inspect(ctx, InspectRequest{
				Hash:         inspectHexHash,
				Title:        "Cancellation",
				MetadataWait: maxMetadataWait,
			})
			done <- result{response: response, err: err}
		}()

		select {
		case <-entered:
			cancel()
		case <-ctx.Done():
			t.Fatal("Inspect did not enter GetTorrentInfo before the test deadline")
		}

		prompt, stopPrompt := context.WithTimeout(context.Background(), time.Second)
		defer stopPrompt()
		select {
		case got := <-done:
			if got.err == nil {
				t.Fatalf("Inspect() error = nil after cancellation, want an error; response = %+v", got.response)
			}
			if got.response != nil {
				t.Errorf("Inspect() response = %+v after cancellation, want nil", got.response)
			}
		case <-prompt.Done():
			t.Fatalf("Inspect did not return within the prompt cancellation deadline: %v", prompt.Err())
		}
	})
}

func TestInspect_FileListContract(t *testing.T) {
	t.Run("I12_preserves_every_file_field_and_GoStorm_order", func(t *testing.T) {
		files := []FileStat{
			{ID: 29, Path: "z-last-by-name.flac", Length: int64(1<<32) + 17},
			{ID: 2, Path: "a-first-by-name.flac", Length: 41},
			{ID: 71, Path: "middle.bin", Length: 9},
		}
		got := inspectSuccessfully(t, files, "Field fidelity")
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want exact ordered projection %+v", got.Files, want)
		}
	})

	t.Run("I13_returns_non_audio_files_without_filtering", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Album/disc.cue", Length: 10},
			{ID: 2, Path: "Album/rip.log", Length: 20},
			{ID: 3, Path: "Album/cover.jpg", Length: 30},
			{ID: 4, Path: "Album/release.nfo", Length: 40},
			{ID: 5, Path: "Album/track.flac", Length: 50},
		}
		got := inspectSuccessfully(t, files, "Non-audio files")
		if want := inspectFilesFromStats(files); !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want all non-audio and audio files %+v", got.Files, want)
		}
	})

	t.Run("I14_genuinely_empty_file_list_is_a_non_null_success", func(t *testing.T) {
		got := inspectSuccessfully(t, []FileStat{}, "Empty torrent")
		if got.Files == nil {
			t.Fatal("Files = nil, want a non-null empty array")
		}
		if len(got.Files) != 0 {
			t.Fatalf("len(Files) = %d, want 0", len(got.Files))
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal(response): %v", err)
		}
		if !strings.Contains(string(encoded), `"files":[]`) {
			t.Errorf("response JSON = %s, want files encoded as [] rather than null", encoded)
		}
	})

	t.Run("I15_single_file_torrent_returns_exactly_one_entry", func(t *testing.T) {
		files := []FileStat{{ID: 17, Path: "only-file.m4b", Length: 123456}}
		got := inspectSuccessfully(t, files, "Single file")
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want exactly %+v", got.Files, want)
		}
	})

	t.Run("I16_zero_length_file_is_not_skipped", func(t *testing.T) {
		files := []FileStat{
			{ID: 4, Path: "Album/zero.log", Length: 0},
			{ID: 5, Path: "Album/audio.flac", Length: 100},
		}
		got := inspectSuccessfully(t, files, "Zero length")
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want zero-length entry preserved in %+v", got.Files, want)
		}
	})

	t.Run("I17_paths_round_trip_verbatim_without_cleaning_or_separator_changes", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Artist Name/Album Name/01 track.flac", Length: 1},
			{ID: 2, Path: "Beyoncé/日本語/Été.m4a", Length: 2},
			{ID: 3, Path: "deeply/nested/directory/track.mp3", Length: 3},
		}
		got := inspectSuccessfully(t, files, "Verbatim paths")
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %#v, want byte-for-byte paths %#v", got.Files, want)
		}
	})

	t.Run("I18_does_not_create_stubs_write_registries_or_run_success_cleanup", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/track.flac", Length: 123}}
		engine := successfulInspectEngine(inspectHexHash, files)
		registry := &inspectRegistrySpy{}
		var sideEffectMu sync.Mutex
		var invalidated, blacklisted []string
		manager := New(Config{
			MoviesDir:     "\x00inspect-must-not-touch-movies",
			TVDir:         "\x00inspect-must-not-touch-tv",
			GoStorm:       engine,
			Registry:      registry,
			AudioRegistry: registry,
			InvalidatePath: func(path string) {
				sideEffectMu.Lock()
				defer sideEffectMu.Unlock()
				invalidated = append(invalidated, path)
			},
			Blacklist: func(path, hash string) {
				sideEffectMu.Lock()
				defer sideEffectMu.Unlock()
				blacklisted = append(blacklisted, path+"|"+hash)
			},
			Logger: log.New(io.Discard, "", 0),
		})

		got, err := manager.Inspect(context.Background(), InspectRequest{Hash: inspectHexHash, Title: "No mutation"})
		if err != nil {
			t.Fatalf("Inspect() error = %v, want nil with deliberately unusable media paths", err)
		}
		if want := inspectFilesFromStats(files); !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want %+v", got.Files, want)
		}
		if calls := registry.snapshot(); len(calls) != 0 {
			t.Errorf("registry calls = %v, want none", calls)
		}
		sideEffectMu.Lock()
		defer sideEffectMu.Unlock()
		if len(invalidated) != 0 || len(blacklisted) != 0 {
			t.Errorf("filesystem/blacklist hook calls = invalidated %q, blacklisted %q; want none", invalidated, blacklisted)
		}
		wantMethods := []string{"ListTorrents", "AddTorrent", "GetTorrentInfo"}
		calls := engine.snapshot()
		gotMethods := make([]string, len(calls))
		for i := range calls {
			gotMethods[i] = calls[i].Method
		}
		if !reflect.DeepEqual(gotMethods, wantMethods) {
			t.Errorf("GoStorm methods = %v, want inspection-only calls %v", gotMethods, wantMethods)
		}
	})

	t.Run("I19_does_not_score_identify_or_select_a_best_file", func(t *testing.T) {
		title := "Perfect Match"
		files := []FileStat{
			{ID: 44, Path: "notes/tiny.log", Length: 7},
			{ID: 3, Path: "Perfect Match.flac", Length: 100},
			{ID: 91, Path: "unrelated-largest-file.bin", Length: int64(1 << 40)},
			{ID: 6, Path: "cover.jpg", Length: 50},
		}
		got := inspectSuccessfully(t, files, title)
		want := inspectFilesFromStats(files)
		if !reflect.DeepEqual(got.Files, want) {
			t.Errorf("Files = %+v, want every file in GoStorm order %+v", got.Files, want)
		}
	})
}

func successfulInspectEngine(hash string, files []FileStat) *inspectGoStorm {
	if files == nil {
		files = []FileStat{}
	}
	return &inspectGoStorm{
		addHash: hash,
		info: &TorrentStats{
			Hash:      hash,
			FileStats: append([]FileStat(nil), files...),
		},
	}
}

func newInspectManager(engine GoStorm, registry *inspectRegistrySpy) *Manager {
	return New(Config{
		GoStorm:       engine,
		Registry:      registry,
		AudioRegistry: registry,
		Logger:        log.New(io.Discard, "", 0),
	})
}

func inspectSuccessfully(t *testing.T, files []FileStat, title string) *InspectResponse {
	t.Helper()
	engine := successfulInspectEngine(inspectHexHash, files)
	response, err := newInspectManager(engine, nil).Inspect(context.Background(), InspectRequest{
		Hash:  inspectHexHash,
		Title: title,
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v, want nil", err)
	}
	if response == nil {
		t.Fatal("Inspect() response = nil, want non-nil")
	}
	if response.Hash != inspectHexHash {
		t.Errorf("response Hash = %q, want %q", response.Hash, inspectHexHash)
	}
	return response
}

func inspectFilesFromStats(files []FileStat) []InspectFile {
	result := make([]InspectFile, len(files))
	for i, file := range files {
		result[i] = InspectFile{SourcePath: file.Path, FileIndex: file.ID, Size: file.Length}
	}
	return result
}

func inspectCallsFor(engine *inspectGoStorm, method string) []inspectEngineCall {
	var result []inspectEngineCall
	for _, call := range engine.snapshot() {
		if call.Method == method {
			result = append(result, call)
		}
	}
	return result
}

func assertInspectStatus(t *testing.T, response *InspectResponse, err error, wantStatus int) {
	t.Helper()
	if response != nil {
		t.Errorf("Inspect() response = %+v, want nil on error", response)
	}
	var libraryErr *Error
	if !errors.As(err, &libraryErr) {
		t.Fatalf("Inspect() error = %v (%T), want *Error with status %d", err, err, wantStatus)
	}
	if libraryErr.Status != wantStatus {
		t.Errorf("Inspect() status = %d, want %d", libraryErr.Status, wantStatus)
	}
}
