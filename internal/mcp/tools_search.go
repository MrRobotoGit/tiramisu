package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/musicimport"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/syncer/engines"
)

type realBackend struct {
	config func() *config.Config
	brainz *musicimport.MusicBrainz // one client: MusicBrainz is rate limited
}

func (b *realBackend) tmdb() *tmdb.Client { return tmdb.NewClient(b.config().TMDBAPIKey) }

func (b *realBackend) SearchMovies(ctx context.Context, q, y string) ([]tmdb.Movie, error) {
	return b.tmdb().SearchMovies(ctx, q, y)
}

func (b *realBackend) SearchTV(ctx context.Context, q, y string) ([]tmdb.TVShow, error) {
	return b.tmdb().SearchTV(ctx, q, y)
}

func (b *realBackend) MovieIMDB(ctx context.Context, id int) (string, error) {
	return b.tmdb().ExternalIDs(ctx, id)
}

func (b *realBackend) ShowIMDB(ctx context.Context, id int) (string, error) {
	return b.tmdb().TVExternalIDs(ctx, id)
}

func (b *realBackend) SearchMovie(ctx context.Context, cfg engines.SearchConfig, imdb, title string, year int) (engines.SearchResult, error) {
	return engines.NewMovieSearcher(cfg).Search(ctx, imdb, title, year)
}

func (b *realBackend) SearchShow(ctx context.Context, cfg engines.SearchConfig, imdb string, tmdbID int, seasons ...int) (engines.SearchResult, error) {
	return engines.NewShowSearcher(cfg).Search(ctx, imdb, tmdbID, seasons...)
}

func (b *realBackend) ShowTitles(ctx context.Context, cfg engines.SearchConfig, tmdbID int) (string, string, []string, error) {
	details, known, err := engines.NewShowSearcher(cfg).KnownTitles(ctx, tmdbID)
	if err != nil {
		return "", "", nil, err
	}
	name := details.Name
	if name == "" {
		name = details.OriginalName
	}
	return name, details.FirstAirDate, known, nil
}

func (b *realBackend) ShowMatcher(cfg engines.SearchConfig, show string, known []string, release string) func([]string) error {
	return engines.NewShowSearcher(cfg).ShowMatcher(show, known, release)
}

func (b *realBackend) FetchTorrent(ctx context.Context, cfg engines.SearchConfig, url string) (library.TorrentSource, error) {
	if !cfg.ProwlarrCfg.Enabled {
		return library.TorrentSource{}, fmt.Errorf("prowlarr is not configured")
	}
	return prowlarr.NewClient(cfg.ProwlarrCfg).FetchTorrent(ctx, url)
}

type ResolveInput struct {
	Query string `json:"query" jsonschema:"title to look up"`
	Year  int    `json:"year,omitempty" jsonschema:"release or first-air year, narrows the match"`
	Type  string `json:"type" jsonschema:"movie or tv"`
}

type Title struct {
	Title        string `json:"title"`
	Year         int    `json:"year,omitempty"`
	IMDB         string `json:"imdb,omitempty"`
	TMDBID       int    `json:"tmdb_id"`
	FirstAirDate string `json:"first_air_date,omitempty"`
}

type ResolveOutput struct {
	Results []Title `json:"results"`
}

const maxResolve = 5

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

func (s *server) registerSearch(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "resolve_title",
		Description: "Find a movie or show on TMDB and return its IMDb id (and TMDB id and first-air date for shows). The first step before search_releases.",
	}, s.resolveTitle)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "search_releases",
		Description: "Start a release search with the deployment's own scoring profile (the same pipeline as the sync: Prowlarr + Torrentio, gates, score). Returns a job_id; poll get_job. Takes minutes on slow indexers.",
	}, s.searchReleases)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "get_job",
		Description: "Report a job started by search_releases, add_release, search_album, add_album, inspect_torrent or add_audio. Waits up to wait_seconds for it to finish.",
	}, s.getJob)
}

func (s *server) resolveTitle(ctx context.Context, _ *sdk.CallToolRequest, in ResolveInput) (*sdk.CallToolResult, ResolveOutput, error) {
	year := ""
	if in.Year > 0 {
		year = strconv.Itoa(in.Year)
	}
	out := ResolveOutput{Results: []Title{}}
	switch in.Type {
	case "movie":
		movies, err := s.deps.Backend.SearchMovies(ctx, in.Query, year)
		if err != nil {
			return errResult(fmt.Errorf("tmdb search: %w", err)), out, nil
		}
		for i, m := range movies {
			if i == maxResolve {
				break
			}
			imdb, _ := s.deps.Backend.MovieIMDB(ctx, m.ID)
			out.Results = append(out.Results, Title{Title: m.Title, Year: yearOf(m.ReleaseDate), IMDB: imdb, TMDBID: m.ID})
		}
	case "tv":
		shows, err := s.deps.Backend.SearchTV(ctx, in.Query, year)
		if err != nil {
			return errResult(fmt.Errorf("tmdb search: %w", err)), out, nil
		}
		for i, sh := range shows {
			if i == maxResolve {
				break
			}
			imdb, _ := s.deps.Backend.ShowIMDB(ctx, sh.ID)
			out.Results = append(out.Results, Title{Title: sh.Name, Year: yearOf(sh.FirstAirDate), IMDB: imdb, TMDBID: sh.ID, FirstAirDate: sh.FirstAirDate})
		}
	default:
		return errResult(fmt.Errorf("type must be movie or tv, got %q", in.Type)), out, nil
	}
	return nil, out, nil
}

type SearchInput struct {
	IMDB      string         `json:"imdb" jsonschema:"IMDb id from resolve_title"`
	Type      string         `json:"type" jsonschema:"movie or tv"`
	Title     string         `json:"title,omitempty" jsonschema:"movie: required, from resolve_title; becomes the file name"`
	Year      int            `json:"year,omitempty" jsonschema:"movie: required, from resolve_title"`
	TMDBID    int            `json:"tmdb_id,omitempty" jsonschema:"required for tv"`
	Seasons   []int          `json:"seasons,omitempty" jsonschema:"tv: seasons to search; default the last two"`
	Overrides map[string]any `json:"overrides,omitempty" jsonschema:"ONLY when the user explicitly asked to change the scoring profile. Partial {movies:{...},tv:{...},language:{...}} with config.json field names; applies to this search only"`
}

type JobStarted struct {
	JobID string `json:"job_id"`
}

// searchJob is what an add needs from the search that found the release.
type searchJob struct {
	Type    string
	IMDB    string
	Title   string
	Year    int
	TMDBID  int
	Config  engines.SearchConfig
	Result  engines.SearchResult
	Applied map[string]any
}

type SearchView struct {
	Type             string                          `json:"type"`
	Candidates       []engines.Candidate             `json:"candidates"`
	Complete         bool                            `json:"complete"`
	HadRaw           bool                            `json:"had_raw"`
	Seasons          map[string]engines.SeasonStatus `json:"seasons,omitempty"`
	Show             string                          `json:"show,omitempty"`
	OverridesApplied map[string]any                  `json:"overrides_applied,omitempty"`
}

type JobInput struct {
	JobID       string `json:"job_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (max 25) for the job to finish"`
}

type JobOutput struct {
	JobID   string                    `json:"job_id"`
	Kind    string                    `json:"kind"`
	Status  string                    `json:"status"`
	Error   string                    `json:"error,omitempty"`
	Search  *SearchView               `json:"search,omitempty"`
	Add     *library.AddResponse      `json:"add,omitempty"`
	Audio   *library.AudioAddResponse `json:"audio,omitempty"`
	Inspect *library.InspectResponse  `json:"inspect,omitempty"`
	Album   *AlbumSearchView          `json:"album,omitempty"`
	Added   *AlbumAdded               `json:"album_added,omitempty"`
}

func (s *server) searchReleases(_ context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, JobStarted, error) {
	if in.IMDB == "" {
		return errResult(fmt.Errorf("imdb is required")), JobStarted{}, nil
	}
	switch in.Type {
	case "movie":
		// add_release builds the movie filename from these: they must come from resolve_title.
		if strings.TrimSpace(in.Title) == "" || in.Year == 0 {
			return errResult(fmt.Errorf("title and year are required for a movie: resolve_title returns them")), JobStarted{}, nil
		}
	case "tv":
		if in.TMDBID == 0 {
			return errResult(fmt.Errorf("tmdb_id is required for tv: resolve_title returns it")), JobStarted{}, nil
		}
	default:
		return errResult(fmt.Errorf("type must be movie or tv, got %q", in.Type)), JobStarted{}, nil
	}
	var raw json.RawMessage
	if len(in.Overrides) > 0 {
		raw, _ = json.Marshal(in.Overrides)
	}
	cfg, applied, err := engines.ApplyOverrides(s.searchConfig(), raw)
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	sj := &searchJob{Type: in.Type, IMDB: in.IMDB, Title: in.Title, Year: in.Year, TMDBID: in.TMDBID, Config: cfg}
	if applied != nil {
		sj.Applied = in.Overrides
	}
	job, err := s.jobs.Start("search", func(ctx context.Context) (any, error) {
		var err error
		if in.Type == "movie" {
			sj.Result, err = s.deps.Backend.SearchMovie(ctx, cfg, in.IMDB, in.Title, in.Year)
		} else {
			sj.Result, err = s.deps.Backend.SearchShow(ctx, cfg, in.IMDB, in.TMDBID, in.Seasons...)
		}
		if err != nil {
			return nil, err
		}
		return sj, nil
	})
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}

func (s *server) getJob(ctx context.Context, _ *sdk.CallToolRequest, in JobInput) (*sdk.CallToolResult, JobOutput, error) {
	wait := min(time.Duration(in.WaitSeconds)*time.Second, maxWait)
	snap, err := s.jobs.Wait(ctx, in.JobID, wait)
	if err != nil {
		return errResult(err), JobOutput{}, nil
	}
	out := JobOutput{JobID: snap.ID, Kind: snap.Kind, Status: snap.Status}
	if snap.Err != nil {
		out.Error = snap.Err.Error()
	}
	switch r := snap.Result.(type) {
	case *searchJob:
		view := &SearchView{
			Type: r.Type, Candidates: r.Result.Candidates, Complete: r.Result.Complete,
			HadRaw: r.Result.HadRaw, Show: r.Result.ShowName, OverridesApplied: r.Applied,
		}
		if len(r.Result.Seasons) > 0 {
			view.Seasons = map[string]engines.SeasonStatus{}
			for n, st := range r.Result.Seasons {
				view.Seasons[strconv.Itoa(n)] = st
			}
		}
		out.Search = view
	case *library.AddResponse:
		out.Add = r
	case *library.AudioAddResponse:
		out.Audio = r
	case *library.InspectResponse:
		out.Inspect = r
	case *albumJob:
		out.Album = albumView(r)
	case *AlbumAdded:
		out.Added = r
	}
	return nil, out, nil
}
