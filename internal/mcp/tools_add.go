package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/library"
	"tiramisu/internal/syncer/engines"
)

type AddInput struct {
	JobID   string `json:"job_id,omitempty" jsonschema:"the search_releases job the release came from"`
	Hash    string `json:"hash,omitempty" jsonschema:"the chosen candidate's hash, or a release the user named"`
	Season  int    `json:"season,omitempty" jsonschema:"tv: overrides the candidate's season"`
	Episode int    `json:"episode,omitempty" jsonschema:"tv: overrides the candidate's episode; 0 = season pack"`

	// Explicit mode: only when the user names a release that is not in a search.
	Magnet       string `json:"magnet,omitempty" jsonschema:"explicit mode"`
	Type         string `json:"type,omitempty" jsonschema:"explicit mode: movie or tv"`
	Title        string `json:"title,omitempty" jsonschema:"explicit mode"`
	ReleaseTitle string `json:"release_title,omitempty" jsonschema:"explicit mode: the raw release name; quality tags in the filename come from it"`
	Year         int    `json:"year,omitempty" jsonschema:"explicit mode, movies"`
	IMDB         string `json:"imdb,omitempty" jsonschema:"explicit mode, movies"`
	TMDBID       int    `json:"tmdb_id,omitempty" jsonschema:"explicit mode, tv: required for the show check"`
}

func (s *server) registerAdd(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "add_release",
		Description: "File the release the user chose. Normal mode: job_id of the search + hash of a candidate; title, year, quality tags and score come from the search. " +
			"Explicit mode (no job_id): only for a release the user named; pass hash or magnet with its metadata. TV releases are checked against the show's names and refused if the files belong to another show. Returns a job_id.",
	}, s.addRelease)
}

func (s *server) addRelease(ctx context.Context, _ *sdk.CallToolRequest, in AddInput) (*sdk.CallToolResult, JobStarted, error) {
	var (
		req      library.AddRequest
		cfg      engines.SearchConfig
		download string
		matcher  func(ctx context.Context) (func([]string) error, error)
	)
	if in.JobID != "" {
		snap, err := s.jobs.Wait(ctx, in.JobID, 0)
		if err != nil {
			return errResult(err), JobStarted{}, nil
		}
		sj, ok := snap.Result.(*searchJob)
		if !ok || snap.Status != StatusDone {
			return errResult(fmt.Errorf("job %s is not a finished search", in.JobID)), JobStarted{}, nil
		}
		var c *engines.Candidate
		for i := range sj.Result.Candidates {
			if strings.EqualFold(sj.Result.Candidates[i].Hash, in.Hash) {
				c = &sj.Result.Candidates[i]
			}
		}
		if c == nil {
			return errResult(fmt.Errorf("hash %s is not among that search's candidates", in.Hash)), JobStarted{}, nil
		}
		cfg, download = sj.Config, c.DownloadURL
		is4K := c.Is4K
		req = library.AddRequest{Hash: c.Hash, ReleaseTitle: c.ReleaseTitle, Is4K: &is4K}
		if sj.Type == "movie" {
			req.Type, req.Title, req.Year, req.IMDB = "movie", sj.Title, sj.Year, sj.IMDB
		} else {
			req.Type, req.Title, req.FirstAirDate = "tv", sj.Result.ShowName, sj.Result.FirstAirDate
			req.Season, req.Episode, req.QualityScore = c.Season, c.Episode, c.QualityScore
			show, known, release := sj.Result.ShowName, sj.Result.KnownTitles, c.ReleaseTitle
			matcher = func(context.Context) (func([]string) error, error) {
				return s.deps.Backend.ShowMatcher(cfg, show, known, release), nil
			}
		}
	} else {
		if in.Hash == "" && in.Magnet == "" {
			return errResult(fmt.Errorf("job_id, or hash/magnet for an explicit add, is required")), JobStarted{}, nil
		}
		cfg = s.searchConfig()
		req = library.AddRequest{
			Type: in.Type, Hash: in.Hash, Magnet: in.Magnet, Title: in.Title,
			ReleaseTitle: in.ReleaseTitle, Year: in.Year, IMDB: in.IMDB,
		}
		if in.Type == "tv" {
			if in.TMDBID == 0 {
				return errResult(fmt.Errorf("tmdb_id is required for an explicit tv add")), JobStarted{}, nil
			}
			tmdbID, release := in.TMDBID, in.ReleaseTitle
			matcher = func(ctx context.Context) (func([]string) error, error) {
				name, first, known, err := s.deps.Backend.ShowTitles(ctx, cfg, tmdbID)
				if err != nil {
					return nil, err
				}
				if req.Title == "" {
					req.Title = name
				}
				req.FirstAirDate = first
				return s.deps.Backend.ShowMatcher(cfg, name, known, release), nil
			}
		}
	}
	if in.Season > 0 {
		req.Season = in.Season
	}
	if in.Episode > 0 {
		req.Episode = in.Episode
	}

	job, err := s.jobs.Start("add", func(ctx context.Context) (any, error) {
		if matcher != nil {
			accept, err := matcher(ctx)
			if err != nil {
				return nil, fmt.Errorf("show check: %w", err)
			}
			req.AcceptFiles = accept
		}
		if download != "" {
			// Only the release's own file: library refuses another hash with a 400.
			if src, err := s.deps.Backend.FetchTorrent(ctx, cfg, download); err == nil &&
				strings.EqualFold(src.Hash, req.Hash) && len(src.File) > 0 {
				req.TorrentFile = src.File
			}
		}
		return s.deps.Library.Add(ctx, req)
	})
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}
