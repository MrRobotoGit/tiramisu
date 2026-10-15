// Package mcp exposes library management as MCP tools at :9080/mcp.
package mcp

import (
	"context"
	"log"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/musicimport"
	"tiramisu/internal/syncer/engines"
)

// Library is the part of *library.Manager the tools use.
type Library interface {
	Add(ctx context.Context, req library.AddRequest) (*library.AddResponse, error)
	AddAudio(ctx context.Context, req library.AddRequest) (*library.AudioAddResponse, error)
	Inspect(ctx context.Context, req library.InspectRequest) (*library.InspectResponse, error)
	List(kind string) ([]library.Item, error)
	ListAudio(req library.AudioListRequest) (*library.AudioListResponse, error)
	ListGaps() ([]library.Gap, int, error)
	RemoveAny(ctx context.Context, req library.RemoveRequest) (any, error)
}

// Backend is the network side: TMDB, the indexers, the release file.
type Backend interface {
	SearchMovies(ctx context.Context, query, year string) ([]tmdb.Movie, error)
	SearchTV(ctx context.Context, query, year string) ([]tmdb.TVShow, error)
	MovieIMDB(ctx context.Context, tmdbID int) (string, error)
	ShowIMDB(ctx context.Context, tmdbID int) (string, error)
	SearchMovie(ctx context.Context, cfg engines.SearchConfig, imdb, title string, year int) (engines.SearchResult, error)
	SearchShow(ctx context.Context, cfg engines.SearchConfig, imdb string, tmdbID int, seasons ...int) (engines.SearchResult, error)
	ShowTitles(ctx context.Context, cfg engines.SearchConfig, tmdbID int) (name, firstAirDate string, known []string, err error)
	ShowMatcher(cfg engines.SearchConfig, showName string, known []string, releaseTitle string) func([]string) error
	FetchTorrent(ctx context.Context, cfg engines.SearchConfig, downloadURL string) (library.TorrentSource, error)

	// Music: MusicBrainz identity, the sync's lossless search, and its filing.
	ArtistAlbums(ctx context.Context, artist string) ([]musicimport.ArtistReleaseGroup, error)
	FindAlbum(ctx context.Context, artist, album string) (musicimport.ReleaseGroup, bool, error)
	AlbumByID(ctx context.Context, rgID string) (musicimport.ReleaseGroup, bool, error)
	CommittedMusic(ctx context.Context) (musicimport.CommittedSet, error)
	SearchAlbum(ctx context.Context, artist, album string) ([]musicimport.Candidate, error)
	AddAlbum(ctx context.Context, artist, album, rgID string, c musicimport.Candidate) (musicimport.AddResult, error)
}

type Deps struct {
	Library  Library
	Backend  Backend // nil = the real one built from Config
	Config   func() *config.Config
	StateDir func() string
	Version  string
	Logger   *log.Logger
}

type server struct {
	deps Deps
	jobs *Jobs
}

const (
	maxJobs    = 20
	jobTTL     = 30 * time.Minute
	jobTimeout = 10 * time.Minute
	maxWait    = 25 * time.Second
)

// NewServer builds the MCP server with every tool registered.
func NewServer(deps Deps) *sdk.Server {
	if deps.Backend == nil {
		deps.Backend = &realBackend{config: deps.Config, brainz: musicimport.NewMusicBrainz()}
	}
	s := &server{deps: deps, jobs: NewJobs(maxJobs, jobTTL, jobTimeout)}
	if deps.Logger != nil {
		s.jobs.logger = deps.Logger
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "tiramisu", Version: deps.Version},
		&sdk.ServerOptions{Instructions: instructions})
	s.registerSearch(srv)
	s.registerAdd(srv)
	s.registerAudio(srv)
	s.registerMusic(srv)
	s.registerLibrary(srv)
	return srv
}

// Register mounts the server on mux at /mcp.
func Register(mux *http.ServeMux, deps Deps) {
	srv := NewServer(deps)
	mux.Handle("/mcp", sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
}

// searchConfig is the live profile, translated as the syncers translate it.
func (s *server) searchConfig() engines.SearchConfig {
	c := s.deps.Config()
	return engines.SearchConfig{
		TMDBAPIKey:   c.TMDBAPIKey,
		TorrentioURL: c.TorrentioURL,
		ProwlarrCfg:  c.Prowlarr,
		Language:     c.Language,
		MovieWeights: c.QualityScoringConfig.MovieWeights(),
		TVWeights:    c.QualityScoringConfig.TVWeights(),
		StateDir:     s.deps.StateDir(),
		Logger:       s.deps.Logger,
	}
}

const instructions = `Tiramisu library tools. The library is virtual: adding a release writes a stub the media server scans; nothing is downloaded up front.

Rules:
- Video: resolve_title, then search_releases, then poll get_job until done, then add_release.
- Music: resolve_album (an artist alone lists the discography, with present=true for albums already in the library), then search_album per album, get_job, then add_album. Only lossless releases are offered: an album available only as MP3 cannot be added, say so.
- A search_album warning means rg_id may name another album: tell the user and resolve_album again before add_album.
- inspect_torrent and add_audio are for a torrent the user names, and for audiobooks, which have no search.
- Present the candidates and wait for the user's choice before add_release, even when there is only one. One line each: name, size, seeders.
- complete=false means the search did not finish (an indexer failed or timed out). Say "the search was incomplete", never "nothing exists".
- A complete search that returns nothing for a well-known title may mean its releases are blacklisted (removed for good in the past). Say so.
- Pass overrides to search_releases only when the user explicitly asked to change the profile in this conversation. Never use them on your own to find something.
- Before remove_entries with confirm=true on movie or tv, show the whole preview and ask whether the removal is for good (blacklist=true) or to replace (blacklist=false) if the user did not say. Music and audiobook have no blacklist and always take blacklist=false: albums the music sync imported stay out, but one added by hand may come back if the music discovery recommends it later. Say so when the user wants it gone for good.
- A remove_entries warning means the entry is gone but was not blacklisted: say the sync may add it back.
- Never re-add the dead_hash of a gap: it is the release the reaper discarded. The engine retries gaps on its own.
- To undo a wrong add: remove_entries on that entry, blacklist=true for movie or tv, false for music and audiobook.
- When the user drops a search or an inspect, or before retrying one, cancel_job the job still running: running jobs share a cap.
- Report the choice, the outcome in one line, and anything that did not go as described. Keep rejected candidates and raw responses for when the user asks.
- A search running while a scheduled sync runs can be slow: both hit the same indexers.`

// errResult is the tool error convention: the message says which half broke.
func errResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}
}
