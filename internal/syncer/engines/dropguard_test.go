package engines

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog/torrentio"
	"tiramisu/internal/config"
	"tiramisu/internal/metadb"
)

const engineDropGuardHash = "abcdef0123456789abcdef0123456789abcdef01"

type goStormRecorder struct {
	mu           sync.Mutex
	torrents     []TorrentStats
	torrentInfo  *TorrentStats
	removeStatus int
	added        []string
	removed      []string
}

func (r *goStormRecorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action, _ := body["action"].(string)
	switch action {
	case "list":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.torrents)
	case "add":
		link, _ := body["link"].(string)
		hash := infoHashFromMagnet(link)
		r.recordAdd(hash)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"hash": hash})
	case "get":
		w.Header().Set("Content-Type", "application/json")
		if r.torrentInfo == nil {
			_ = json.NewEncoder(w).Encode(TorrentStats{})
			return
		}
		_ = json.NewEncoder(w).Encode(r.torrentInfo)
	case "rem":
		hash, _ := body["hash"].(string)
		r.recordRemove(hash)
		r.mu.Lock()
		status := r.removeStatus
		r.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}

func infoHashFromMagnet(magnet string) string {
	match := regexp.MustCompile(`(?i)btih:([a-f0-9]{40})`).FindStringSubmatch(magnet)
	if len(match) != 2 {
		return ""
	}
	return strings.ToLower(match[1])
}

func (r *goStormRecorder) recordAdd(hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, hash)
}

func (r *goStormRecorder) recordRemove(hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, hash)
}

func (r *goStormRecorder) addedHashes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.added...)
}

func (r *goStormRecorder) removedHashes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonHTTPResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type refreshRecorder struct {
	mu    sync.Mutex
	calls int
}

func (r *refreshRecorder) RefreshLibrary(context.Context, int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *refreshRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newDropGuardGoStorm(t *testing.T, recorder *goStormRecorder) *GoStormClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(recorder.serveHTTP))
	t.Cleanup(server.Close)
	return NewGoStormClient(server.URL)
}

func openEngineDropGuardDB(t *testing.T, referenced bool) *metadb.DB {
	t.Helper()
	db, err := metadb.New(filepath.Join(t.TempDir(), "registry.db"), nil)
	if err != nil {
		t.Fatalf("open metadb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if !referenced {
		return db
	}
	projection := metadb.AudioProjection{
		Section:         "music",
		VirtualPath:     "Artist/Album/Track.flac",
		PortablePathKey: "artist/album/track.flac",
		Hash:            engineDropGuardHash,
		FileIndex:       1,
		SourcePath:      "source/Track.flac",
		Size:            1234,
		MtimeNS:         100,
		Title:           "Track",
		Magnet:          "magnet:?xt=urn:btih:" + engineDropGuardHash,
		TxnID:           "txn-engine-guard",
		StagingName:     ".txn-engine-guard",
		CreatedAtNS:     100,
		UpdatedAtNS:     100,
	}
	if err := db.StageAudioProjections(projection.TxnID, []metadb.AudioProjection{projection}); err != nil {
		t.Fatalf("stage audio projection: %v", err)
	}
	if n, err := db.CommitAudioProjections(projection.TxnID, 101); err != nil || n != 1 {
		t.Fatalf("commit audio projection = (%d, %v), want (1, nil)", n, err)
	}
	return db
}

func closedEngineDropGuardDB(t *testing.T) *metadb.DB {
	t.Helper()
	db := openEngineDropGuardDB(t, false)
	if err := db.Close(); err != nil {
		t.Fatalf("close metadb for error fixture: %v", err)
	}
	return db
}

func writeDropGuardStub(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func requireStubRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stub %q still exists or cannot be checked: %v", path, err)
	}
}

func TestMovieRemoveStubHonorsAudioOwnership(t *testing.T) {
	tests := []struct {
		name         string
		db           func(*testing.T) *metadb.DB
		removeStatus int
		wantRemoved  []string
	}{
		{
			name: "K1 committed audio owner retains torrent",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
		},
		{
			name:        "K2 no audio owner preserves torrent removal",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name:        "K5 L3 nil registry preserves removal without panic",
			db:          func(*testing.T) *metadb.DB { return nil },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name: "L1 registry error retains torrent and still removes stub",
			db:   closedEngineDropGuardDB,
		},
		{
			name:         "L2 RemoveTorrent failure still removes stub",
			db:           func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			removeStatus: http.StatusInternalServerError,
			wantRemoved:  []string{engineDropGuardHash},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &goStormRecorder{removeStatus: tt.removeStatus}
			engine := &MovieGoEngine{
				gostorm: newDropGuardGoStorm(t, recorder),
				logger:  log.New(io.Discard, "", 0),
				db:      tt.db(t),
			}
			stub := writeDropGuardStub(t, "movie.mkv")

			engine.removeStub(context.Background(), stub, engineDropGuardHash)

			requireStubRemoved(t, stub)
			if got := recorder.removedHashes(); !reflect.DeepEqual(got, tt.wantRemoved) {
				t.Errorf("RemoveTorrent calls = %q, want %q", got, tt.wantRemoved)
			}
		})
	}
}

func TestTVRemoveStubHonorsAudioOwnership(t *testing.T) {
	tests := []struct {
		name         string
		db           func(*testing.T) *metadb.DB
		removeStatus int
		wantRemoved  []string
	}{
		{
			name: "TV removeStub shared audio hash retains torrent",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
		},
		{
			name:        "TV removeStub unowned hash preserves removal",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name:        "K5 L3 nil registry preserves TV removal without panic",
			db:          func(*testing.T) *metadb.DB { return nil },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name: "L1 registry error retains torrent and TV stub work continues",
			db:   closedEngineDropGuardDB,
		},
		{
			name:         "L2 RemoveTorrent failure still removes TV stub",
			db:           func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			removeStatus: http.StatusInternalServerError,
			wantRemoved:  []string{engineDropGuardHash},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &goStormRecorder{removeStatus: tt.removeStatus}
			engine := &TVGoEngine{
				gostorm: newDropGuardGoStorm(t, recorder),
				logger:  log.New(io.Discard, "", 0),
				db:      tt.db(t),
			}
			stub := writeDropGuardStub(t, "episode.mkv")

			engine.removeStub(context.Background(), stub, engineDropGuardHash)

			requireStubRemoved(t, stub)
			if got := recorder.removedHashes(); !reflect.DeepEqual(got, tt.wantRemoved) {
				t.Errorf("RemoveTorrent calls = %q, want %q", got, tt.wantRemoved)
			}
		})
	}
}

func TestTVReaperWholePackHonorsAudioOwnership(t *testing.T) {
	tests := []struct {
		name        string
		db          func(*testing.T) *metadb.DB
		wantRemoved []string
	}{
		{
			name: "K3 committed audio owner suppresses whole-pack drop",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
		},
		{
			name:        "K3 no audio owner preserves whole-pack drop",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name:        "K5 L3 nil registry preserves whole-pack drop without panic",
			db:          func(*testing.T) *metadb.DB { return nil },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name: "L1 registry error suppresses drop while reaper deletes stub",
			db:   closedEngineDropGuardDB,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &goStormRecorder{}
			stub := writeDropGuardStub(t, "Show_S01E01.mkv")
			const episodeKey = "show_s01e01"
			engine := &TVGoEngine{
				gostorm: newDropGuardGoStorm(t, recorder),
				logger:  log.New(io.Discard, "", 0),
				db:      tt.db(t),
				registry: map[string]TVEpisodeEntry{
					episodeKey: {Hash: engineDropGuardHash, FilePath: stub},
				},
			}
			pack := deadPack{
				Hash:    engineDropGuardHash,
				Seasons: []int{1},
				Episodes: []metadb.EpisodeEntry{
					{EpisodeKey: episodeKey, Hash: engineDropGuardHash, FilePath: stub},
				},
			}

			engine.dropReplacedPack(context.Background(), pack, map[int]seasonSearch{
				1: {Complete: true, RawSeen: true},
			})

			requireStubRemoved(t, stub)
			if _, ok := engine.registry[episodeKey]; ok {
				t.Error("reaper retained registry entry after deleting its stub")
			}
			if got := recorder.removedHashes(); !reflect.DeepEqual(got, tt.wantRemoved) {
				t.Errorf("RemoveTorrent calls = %q, want %q", got, tt.wantRemoved)
			}
		})
	}
}

func TestTVOrphanSweepHonorsAudioOwnership(t *testing.T) {
	tests := []struct {
		name        string
		db          func(*testing.T) *metadb.DB
		wantRemoved []string
	}{
		{
			name: "K4 committed audio owner suppresses orphan drop",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
		},
		{
			name:        "K4 no audio owner preserves orphan drop",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name:        "K5 L3 nil registry preserves orphan drop without panic",
			db:          func(*testing.T) *metadb.DB { return nil },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name: "L1 registry error suppresses orphan drop and sweep continues",
			db:   closedEngineDropGuardDB,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &goStormRecorder{torrents: []TorrentStats{
				{Hash: engineDropGuardHash, Title: "Example Show Season 1 Complete"},
			}}
			engine := &TVGoEngine{
				gostorm:  newDropGuardGoStorm(t, recorder),
				logger:   log.New(io.Discard, "", 0),
				db:       tt.db(t),
				tvDir:    t.TempDir(),
				registry: make(map[string]TVEpisodeEntry),
			}

			engine.cleanupOrphanedTorrents(context.Background())

			if got := recorder.removedHashes(); !reflect.DeepEqual(got, tt.wantRemoved) {
				t.Errorf("RemoveTorrent calls = %q, want %q", got, tt.wantRemoved)
			}
		})
	}
}

func TestDropTorrentGuardedReportsRemovalOutcome(t *testing.T) {
	tests := []struct {
		name         string
		db           func(*testing.T) *metadb.DB
		hash         string
		removeStatus int
		wantRemoved  bool
		wantErr      bool
		wantCalls    []string
	}{
		{
			name:        "S4 clean removal reports removed",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			hash:        engineDropGuardHash,
			wantRemoved: true,
			wantCalls:   []string{engineDropGuardHash},
		},
		{
			name: "S5 audio ownership withholds without error",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
			hash: engineDropGuardHash,
		},
		{
			name: "S6 registry error withholds without removal",
			db:   closedEngineDropGuardDB,
			hash: engineDropGuardHash,
		},
		{
			name:         "S7 engine error reports not removed and returns error",
			db:           func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			hash:         engineDropGuardHash,
			removeStatus: http.StatusInternalServerError,
			wantErr:      true,
			wantCalls:    []string{engineDropGuardHash},
		},
		{
			name: "U1 empty hash is never sent to engine",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			hash: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &goStormRecorder{removeStatus: tt.removeStatus}
			removed, err := dropTorrentGuarded(
				context.Background(), newDropGuardGoStorm(t, recorder), tt.db(t), nil, tt.hash,
			)
			if removed != tt.wantRemoved {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantError %v", err, tt.wantErr)
			}
			if got := recorder.removedHashes(); !reflect.DeepEqual(got, tt.wantCalls) {
				t.Errorf("RemoveTorrent calls = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

func TestEngineDropMethodsReportRemovalOutcome(t *testing.T) {
	recorder := &goStormRecorder{}
	gs := newDropGuardGoStorm(t, recorder)
	engines := []struct {
		name string
		drop func(context.Context, string) (bool, error)
	}{
		{
			name: "movie",
			drop: (&MovieGoEngine{gostorm: gs}).dropTorrent,
		},
		{
			name: "TV",
			drop: (&TVGoEngine{gostorm: gs}).dropTorrent,
		},
		{
			name: "watchlist",
			drop: (&WatchlistGoEngine{gostorm: gs}).dropTorrent,
		},
	}
	for _, engine := range engines {
		t.Run(engine.name, func(t *testing.T) {
			removed, err := engine.drop(context.Background(), engineDropGuardHash)
			if err != nil || !removed {
				t.Errorf("S4 dropTorrent = (%v, %v), want (true, nil)", removed, err)
			}
		})
	}
	if got := recorder.removedHashes(); !reflect.DeepEqual(got, []string{
		engineDropGuardHash, engineDropGuardHash, engineDropGuardHash,
	}) {
		t.Errorf("adapter RemoveTorrent calls = %q, want one per engine", got)
	}
}

func TestTVOrphanSweepCountsOnlyActualRemovals(t *testing.T) {
	const unownedHash = "fedcba9876543210fedcba9876543210fedcba98"
	recorder := &goStormRecorder{torrents: []TorrentStats{
		{Hash: engineDropGuardHash, Title: "Owned Show Season 1 Complete"},
		{Hash: unownedHash, Title: "Unowned Show Season 1 Complete"},
	}}
	engine := &TVGoEngine{
		gostorm:  newDropGuardGoStorm(t, recorder),
		logger:   log.New(io.Discard, "", 0),
		db:       openEngineDropGuardDB(t, true),
		tvDir:    t.TempDir(),
		registry: make(map[string]TVEpisodeEntry),
	}

	removed := engine.cleanupOrphanedTorrents(context.Background())

	if removed != 1 {
		t.Errorf("T2 orphan removed count = %d, want 1 actual removal", removed)
	}
	if got := recorder.removedHashes(); !reflect.DeepEqual(got, []string{unownedHash}) {
		t.Errorf("T2 RemoveTorrent calls = %q, want only unowned hash %q", got, unownedHash)
	}
}

func newWatchlistTestEngine(
	t *testing.T,
	gs *GoStormClient,
	db *metadb.DB,
	streamsJSON string,
	refresh *refreshRecorder,
) (*WatchlistGoEngine, string) {
	t.Helper()
	torrentioServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, streamsJSON)
	}))
	t.Cleanup(torrentioServer.Close)

	watchlistJSON := `{"MediaContainer":{"Metadata":[{"title":"Shared Movie","year":2026,"type":"movie","Guid":[{"id":"imdb://tt1234567"}]}]}}`
	watchlistClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonHTTPResponse(watchlistJSON), nil
	})}
	moviesDir := t.TempDir()
	return &WatchlistGoEngine{
		gostorm:       gs,
		torrentio:     torrentio.NewClient(torrentioServer.URL, "test"),
		mediasrv:      refresh,
		httpClient:    watchlistClient,
		moviesDir:     moviesDir,
		limiter:       rate.NewLimiter(rate.Inf, 1),
		logger:        log.New(io.Discard, "", 0),
		weights:       config.DefaultMovieWeights(),
		preferredLang: regexp.MustCompile(`$^`),
		db:            db,
	}, moviesDir
}

func newCanceledMetadataGoStorm(cancel context.CancelFunc, recorder *goStormRecorder) *GoStormClient {
	client := NewGoStormClient("http://gostorm.test")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		var body map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		action, _ := body["action"].(string)
		switch action {
		case "add":
			link, _ := body["link"].(string)
			hash := infoHashFromMagnet(link)
			recorder.recordAdd(hash)
			cancel()
			return jsonHTTPResponse(`{"hash":"` + hash + `"}`), nil
		case "rem":
			hash, _ := body["hash"].(string)
			recorder.recordRemove(hash)
			return jsonHTTPResponse(`{}`), nil
		default:
			return nil, errors.New("unexpected GoStorm action after cancellation")
		}
	})}
	return client
}

func TestWatchlistRollbackHonorsPreExistingAudioOwnership(t *testing.T) {
	const streamsJSON = `{"streams":[{"infoHash":"` + engineDropGuardHash + `","title":"Shared Movie 1080p 👤 50 💾 8 GB","name":"1080p"}]}`
	branches := []struct {
		name string
		info *TorrentStats
	}{
		{name: "metadata failure"},
		{
			name: "BDMV detection",
			info: &TorrentStats{Hash: engineDropGuardHash, FileStats: []FileStat{{ID: 1, Path: "Movie/BDMV/index.bdmv", Length: 8 << 30}}},
		},
		{
			name: "no playable MKV",
			info: &TorrentStats{Hash: engineDropGuardHash, FileStats: []FileStat{{ID: 1, Path: "Movie/movie.mp4", Length: 8 << 30}}},
		},
	}
	ownerships := []struct {
		name        string
		db          func(*testing.T) *metadb.DB
		wantRemoved []string
	}{
		{
			name: "S8 audio owned hash first",
			db:   func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, true) },
		},
		{
			name:        "S9 no audio owner",
			db:          func(t *testing.T) *metadb.DB { return openEngineDropGuardDB(t, false) },
			wantRemoved: []string{engineDropGuardHash},
		},
		{
			name:        "S10 nil DB preserves rollback",
			db:          func(*testing.T) *metadb.DB { return nil },
			wantRemoved: []string{engineDropGuardHash},
		},
	}
	for _, branch := range branches {
		for _, ownership := range ownerships {
			t.Run(branch.name+"/"+ownership.name, func(t *testing.T) {
				db := ownership.db(t)
				if ownership.name == "S8 audio owned hash first" {
					referenced, err := db.AudioHashReferenced(engineDropGuardHash)
					if err != nil || !referenced {
						t.Fatalf("precondition: audio ownership before watchlist AddTorrent = (%v, %v), want (true, nil)", referenced, err)
					}
				}
				recorder := &goStormRecorder{torrentInfo: branch.info}
				ctx := context.Background()
				gs := newDropGuardGoStorm(t, recorder)
				if branch.name == "metadata failure" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					gs = newCanceledMetadataGoStorm(cancel, recorder)
				}
				engine, _ := newWatchlistTestEngine(t, gs, db, streamsJSON, &refreshRecorder{})

				err := engine.Run(ctx)
				if branch.name == "metadata failure" {
					if !errors.Is(err, context.Canceled) {
						t.Errorf("Run error = %v, want context.Canceled after deterministic metadata failure", err)
					}
				} else if err != nil {
					t.Errorf("Run error = %v, want nil", err)
				}
				if got := recorder.addedHashes(); !reflect.DeepEqual(got, []string{engineDropGuardHash}) {
					t.Errorf("AddTorrent calls = %q, want existing shared hash %q", got, engineDropGuardHash)
				}
				if got := recorder.removedHashes(); !reflect.DeepEqual(got, ownership.wantRemoved) {
					t.Errorf("RemoveTorrent calls = %q, want %q", got, ownership.wantRemoved)
				}
			})
		}
	}
}

func TestWatchlistNilDBPreservesSuccessfulAddAndRefresh(t *testing.T) {
	const streamsJSON = `{"streams":[` +
		`{"infoHash":"1111111111111111111111111111111111111111","title":"Low Seed 1080p 👤 1 💾 8 GB","name":"rejected"},` +
		`{"infoHash":"` + engineDropGuardHash + `","title":"Chosen 1080p 👤 50 💾 8 GB","name":"chosen"}` +
		`]}`
	recorder := &goStormRecorder{torrentInfo: &TorrentStats{
		Hash:      engineDropGuardHash,
		FileStats: []FileStat{{ID: 1, Path: "Shared.Movie.2026.mkv", Length: 8 << 30}},
	}}
	refresh := &refreshRecorder{}
	engine, moviesDir := newWatchlistTestEngine(t, newDropGuardGoStorm(t, recorder), nil, streamsJSON, refresh)

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("T4 Run with nil DB: %v", err)
	}
	if got := recorder.addedHashes(); !reflect.DeepEqual(got, []string{engineDropGuardHash}) {
		t.Errorf("T4 selected AddTorrent hashes = %q, want only scored winner %q", got, engineDropGuardHash)
	}
	if got := recorder.removedHashes(); len(got) != 0 {
		t.Errorf("T4 successful watchlist add unexpectedly rolled back hashes %q", got)
	}
	stubs, err := filepath.Glob(filepath.Join(moviesDir, "*.mkv"))
	if err != nil {
		t.Fatalf("T4 list created stubs: %v", err)
	}
	if len(stubs) != 1 {
		t.Fatalf("T4 created stubs = %q, want exactly one", stubs)
	}
	stub, err := os.ReadFile(stubs[0])
	if err != nil {
		t.Fatalf("T4 read created stub: %v", err)
	}
	if !strings.Contains(string(stub), engineDropGuardHash) {
		t.Errorf("T4 created stub does not reference selected hash: %s", stub)
	}
	if got := refresh.callCount(); got != 1 {
		t.Errorf("T4 media library refresh calls = %d, want 1", got)
	}
}
