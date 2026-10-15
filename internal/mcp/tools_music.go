package mcp

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"tiramisu/internal/musicimport"
	"tiramisu/internal/prowlarr"
)

// musicCandidates is how many lossless releases a search offers per album.
const musicCandidates = 5

func (b *realBackend) musicLogf() func(string, ...any) {
	return log.New(os.Stdout, "[MCPMusic] ", log.LstdFlags).Printf
}

// musicLibrary is the Library API the music sync files through: same path, same result.
func (b *realBackend) musicLibrary() *musicimport.Tiramisu {
	return musicimport.NewTiramisu(fmt.Sprintf("http://127.0.0.1:%d", b.config().MetricsPort))
}

func (b *realBackend) ArtistAlbums(ctx context.Context, artist string) ([]musicimport.ArtistReleaseGroup, error) {
	id, ok, err := b.brainz.SearchArtist(ctx, artist)
	if err != nil {
		return nil, fmt.Errorf("musicbrainz artist search: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("no MusicBrainz artist named %q", artist)
	}
	return b.brainz.ArtistAlbums(ctx, id, b.config().MusicDiscovery.AlbumTypes)
}

func (b *realBackend) FindAlbum(ctx context.Context, artist, album string) (musicimport.ReleaseGroup, bool, error) {
	return b.brainz.SearchReleaseGroup(ctx, artist, album)
}

func (b *realBackend) CommittedMusic(ctx context.Context) (musicimport.CommittedSet, error) {
	return b.musicLibrary().Committed(ctx)
}

func (b *realBackend) SearchAlbum(ctx context.Context, artist, album string) ([]musicimport.Candidate, error) {
	c := b.config()
	if !c.Prowlarr.Enabled {
		return nil, fmt.Errorf("prowlarr is not configured: music has no other source")
	}
	d := c.MusicDiscovery
	return musicimport.SearchAlbumCandidates(ctx, prowlarr.NewClient(c.Prowlarr), nil, artist, album,
		d.MinSeeders, int64(d.MaxSizeGB*float64(1<<30)), musicCandidates, b.musicLogf()), nil
}

func (b *realBackend) AddAlbum(ctx context.Context, artist, album, rgID string, cand musicimport.Candidate) (musicimport.AddResult, error) {
	c := b.config()
	if !c.Prowlarr.Enabled {
		return musicimport.AddResult{}, fmt.Errorf("prowlarr is not configured: music has no other source")
	}
	return musicimport.AddAlbum(ctx, b.musicLibrary(), b.brainz, prowlarr.NewClient(c.Prowlarr), artist, album, rgID, cand,
		musicimport.IDStyleForPlayer(c.MediaServerType), b.musicLogf())
}

type AlbumInput struct {
	Artist string `json:"artist"`
	Album  string `json:"album,omitempty" jsonschema:"omit to list the artist's discography"`
}

type Album struct {
	RGID    string `json:"rg_id"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Year    int    `json:"year,omitempty"`
	Type    string `json:"type,omitempty"`
	Present bool   `json:"present"`
}

type AlbumsOutput struct {
	Albums []Album `json:"albums"`
	// Warning says present could not be checked: every false is then unknown.
	Warning string `json:"warning,omitempty"`
}

type AlbumSearchInput struct {
	Artist string `json:"artist"`
	Album  string `json:"album"`
	RGID   string `json:"rg_id" jsonschema:"from resolve_album"`
}

type AlbumCandidate struct {
	Hash    string `json:"hash"`
	Name    string `json:"name"`
	SizeMB  int64  `json:"size_mb"`
	Seeders int    `json:"seeders"`
}

type AlbumSearchView struct {
	Artist     string           `json:"artist"`
	Album      string           `json:"album"`
	RGID       string           `json:"rg_id"`
	Candidates []AlbumCandidate `json:"candidates"`
}

type AlbumAdded struct {
	AlreadyPresent bool     `json:"already_present"`
	Files          []string `json:"files"`
}

// albumJob is what add_album needs from the search that found the release.
type albumJob struct {
	Artist, Album, RGID string
	Candidates          []musicimport.Candidate
}

type AddAlbumInput struct {
	JobID string `json:"job_id" jsonschema:"the search_album job"`
	Hash  string `json:"hash" jsonschema:"the chosen candidate"`
}

func (s *server) registerMusic(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "resolve_album",
		Description: "MusicBrainz identity of an album, or with no album the artist's discography (album types from music_discovery, no live or compilation). present=true means it is already in the library.",
	}, s.resolveAlbum)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "search_album",
		Description: "Start the music sync's own search for one album: Prowlarr, lossless only (FLAC), seeder floor and size cap from music_discovery. An album released only as MP3 returns no candidate. Returns a job_id.",
	}, s.searchAlbum)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "add_album",
		Description: "File the album release the user chose, exactly as the music sync would: MusicBrainz track ids, library naming, cue images split per track. Returns a job_id.",
	}, s.addAlbum)
}

func (s *server) resolveAlbum(ctx context.Context, _ *sdk.CallToolRequest, in AlbumInput) (*sdk.CallToolResult, AlbumsOutput, error) {
	out := AlbumsOutput{Albums: []Album{}}
	if strings.TrimSpace(in.Artist) == "" {
		return errResult(fmt.Errorf("artist is required")), out, nil
	}
	if strings.TrimSpace(in.Album) != "" {
		g, ok, err := s.deps.Backend.FindAlbum(ctx, in.Artist, in.Album)
		if err != nil {
			return errResult(fmt.Errorf("musicbrainz: %w", err)), out, nil
		}
		if ok {
			out.Albums = append(out.Albums, Album{RGID: g.ID, Title: g.Title, Artist: g.Artist})
		}
	} else {
		groups, err := s.deps.Backend.ArtistAlbums(ctx, in.Artist)
		if err != nil {
			return errResult(err), out, nil
		}
		for _, g := range groups {
			if len(g.SecondaryTypes) > 0 {
				continue
			}
			out.Albums = append(out.Albums, Album{RGID: g.ID, Title: g.Title, Artist: g.Artist, Year: yearOf(g.FirstReleaseDate), Type: g.PrimaryType})
		}
		sort.SliceStable(out.Albums, func(i, j int) bool { return out.Albums[i].Year < out.Albums[j].Year })
	}
	committed, err := s.deps.Backend.CommittedMusic(ctx)
	if err != nil {
		out.Warning = "the library could not be read: present=false means unknown, not missing (" + err.Error() + ")"
		return nil, out, nil
	}
	for i := range out.Albums {
		a := &out.Albums[i]
		a.Present = committed.HasAlbumNamed(a.Artist, a.Title, a.RGID)
	}
	return nil, out, nil
}

func (s *server) searchAlbum(_ context.Context, _ *sdk.CallToolRequest, in AlbumSearchInput) (*sdk.CallToolResult, JobStarted, error) {
	if strings.TrimSpace(in.Artist) == "" || strings.TrimSpace(in.Album) == "" || in.RGID == "" {
		return errResult(fmt.Errorf("artist, album and rg_id are required: resolve_album returns them")), JobStarted{}, nil
	}
	job, err := s.jobs.Start("search_album", func(ctx context.Context) (any, error) {
		cands, err := s.deps.Backend.SearchAlbum(ctx, in.Artist, in.Album)
		if err != nil {
			return nil, err
		}
		return &albumJob{Artist: in.Artist, Album: in.Album, RGID: in.RGID, Candidates: cands}, nil
	})
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}

func (s *server) addAlbum(ctx context.Context, _ *sdk.CallToolRequest, in AddAlbumInput) (*sdk.CallToolResult, JobStarted, error) {
	snap, err := s.jobs.Wait(ctx, in.JobID, 0)
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	aj, ok := snap.Result.(*albumJob)
	if !ok || snap.Status != StatusDone {
		return errResult(fmt.Errorf("job %s is not a finished search_album", in.JobID)), JobStarted{}, nil
	}
	var chosen *musicimport.Candidate
	for i := range aj.Candidates {
		if strings.EqualFold(aj.Candidates[i].Hash, in.Hash) {
			chosen = &aj.Candidates[i]
		}
	}
	if chosen == nil {
		return errResult(fmt.Errorf("hash %s is not among that search's candidates", in.Hash)), JobStarted{}, nil
	}
	cand := *chosen
	job, err := s.jobs.Start("add_album", func(ctx context.Context) (any, error) {
		res, err := s.deps.Backend.AddAlbum(ctx, aj.Artist, aj.Album, aj.RGID, cand)
		if err != nil {
			return nil, err
		}
		added := &AlbumAdded{AlreadyPresent: res.AlreadyPresent, Files: []string{}}
		for _, f := range res.Files {
			added.Files = append(added.Files, f.Path)
		}
		return added, nil
	})
	if err != nil {
		return errResult(err), JobStarted{}, nil
	}
	return nil, JobStarted{JobID: job.ID}, nil
}

func albumView(aj *albumJob) *AlbumSearchView {
	v := &AlbumSearchView{Artist: aj.Artist, Album: aj.Album, RGID: aj.RGID, Candidates: []AlbumCandidate{}}
	for _, c := range aj.Candidates {
		v.Candidates = append(v.Candidates, AlbumCandidate{Hash: c.Hash, Name: c.Title, SizeMB: c.Size >> 20, Seeders: c.Seeders})
	}
	return v
}
