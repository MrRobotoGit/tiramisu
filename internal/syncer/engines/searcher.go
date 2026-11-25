package engines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/catalog/torrentio"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/prowlarr"
)

// torrentioOptions is the filter both engines and the searchers ask Torrentio for.
const torrentioOptions = "sort=qualitysize|qualityfilter=480p,720p,scr,cam"

// prowlarrClients holds one client per configuration, shared by the engines and every
// MCP searcher: the indexer capabilities and what each indexer has shown about IMDb
// searches are learned once, not per search.
var prowlarrClients sync.Map // prowlarr.ConfigProwlarr -> *prowlarr.Client

func newProwlarrClient(cfg prowlarr.ConfigProwlarr) *prowlarr.Client {
	if !cfg.Enabled {
		return nil
	}
	if c, ok := prowlarrClients.Load(cfg); ok {
		return c.(*prowlarr.Client)
	}
	c, _ := prowlarrClients.LoadOrStore(cfg, prowlarr.NewClient(cfg))
	return c.(*prowlarr.Client)
}

// SearchConfig is what a searcher reads: no directories, no DB, no caches.
type SearchConfig struct {
	TMDBAPIKey   string
	TorrentioURL string
	ProwlarrCfg  prowlarr.ConfigProwlarr
	Language     config.LanguageConfig
	MovieWeights config.MovieWeights
	TVWeights    config.TVWeights
	StateDir     string // blacklist.json, read only
	Logger       *log.Logger
}

type Candidate struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"` // first line of the release title
	ReleaseTitle string  `json:"-"`    // full title, what the filename builders parse
	DownloadURL  string  `json:"-"`
	SizeGB       float64 `json:"size_gb"`
	Seeders      int     `json:"seeders"`
	Is4K         bool    `json:"is_4k"`
	Season       int     `json:"season,omitempty"`
	Episode      int     `json:"episode,omitempty"` // TV: 0 = season pack
	Score        int     `json:"score"`             // TV: Priority, the sort key
	QualityScore int     `json:"-"`                 // TV: registry score, no pack bonus
}

type SeasonStatus struct {
	Complete bool `json:"complete"`
	RawSeen  bool `json:"raw_seen"`
}

type SearchResult struct {
	Candidates   []Candidate          `json:"candidates"`
	Complete     bool                 `json:"complete"`
	HadRaw       bool                 `json:"had_raw"`
	Seasons      map[int]SeasonStatus `json:"seasons,omitempty"`
	ShowName     string               `json:"show_name,omitempty"`
	FirstAirDate string               `json:"first_air_date,omitempty"`
	KnownTitles  []string             `json:"-"`
}

func searchLogger(cfg SearchConfig) *log.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return log.New(os.Stdout, "[LibrarySearch] ", log.LstdFlags)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// searchResolveBudget is how many candidates listed without a hash a search resolves,
// best first. Each costs a round trip through the indexer; the rest are left out, since
// add_release names a candidate by its hash.
const searchResolveBudget = 10

// resolveCandidates gives the best hashless candidates their hash, in score order, and
// drops the ones left without one, the duplicates resolution reveals and the hashes
// reject refuses. cands must already be sorted best first. failed counts the links that
// were tried and gave no hash.
func resolveCandidates(ctx context.Context, pr *prowlarr.Client, cands []Candidate, reject func(string) bool, logf func(string, ...any)) (out []Candidate, failed int) {
	var picked []int
	for i, c := range cands {
		if c.Hash == "" && c.DownloadURL != "" && len(picked) < searchResolveBudget {
			picked = append(picked, i)
		}
	}
	if len(picked) > 0 && pr != nil {
		sem := make(chan struct{}, 5)
		var wg sync.WaitGroup
		for _, i := range picked {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if h, _, ok := fetchRelease(ctx, pr, "", cands[i].ReleaseTitle, cands[i].DownloadURL, logf); ok {
					cands[i].Hash = h
				}
			}()
		}
		wg.Wait()
	}
	out = make([]Candidate, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	dropped := 0
	for _, i := range picked {
		if cands[i].Hash == "" {
			failed++
		}
	}
	for _, c := range cands {
		h := strings.ToLower(c.Hash)
		if h == "" {
			dropped++
			continue
		}
		if seen[h] || (reject != nil && reject(h)) {
			continue
		}
		seen[h] = true
		c.Hash = h
		out = append(out, c)
	}
	if dropped > 0 {
		logf("[Search] %d candidate(s) without a hash left out (%d link(s) failed, budget %d)", dropped, failed, searchResolveBudget)
	}
	return out, failed
}

// finishCandidates resolves out's candidates. Nothing left because links failed is a
// degraded search, not proof nothing exists, exactly as before resolution was deferred.
func finishCandidates(ctx context.Context, pr *prowlarr.Client, out *SearchResult, reject func(string) bool, logf func(string, ...any)) {
	var failed int
	out.Candidates, failed = resolveCandidates(ctx, pr, out.Candidates, reject, logf)
	if len(out.Candidates) == 0 && failed > 0 {
		out.Complete = false
		for season, st := range out.Seasons {
			st.Complete = false
			out.Seasons[season] = st
		}
	}
}

// MovieSearcher runs getMovieStreams on an engine that holds only what it reads.
type MovieSearcher struct{ engine *MovieGoEngine }

func NewMovieSearcher(cfg SearchConfig) *MovieSearcher {
	e := &MovieGoEngine{
		torrentio:     torrentio.NewClient(cfg.TorrentioURL, torrentioOptions),
		prowlarr:      newProwlarrClient(cfg.ProwlarrCfg),
		limiter:       rate.NewLimiter(rate.Every(250*time.Millisecond), 1),
		logger:        searchLogger(cfg),
		blacklistFile: filepath.Join(cfg.StateDir, "blacklist.json"),
		reITA:         CompileLanguageRegex(cfg.Language.PreferredTerms, cfg.Language.PreferredFlags),
		reExclLang:    CompileLanguageRegex(ExcludedTitleTerms(cfg.Language.ExcludedFlags), cfg.Language.ExcludedFlags),
		exclLanguages: ExcludedLanguageSet(cfg.Language.ExcludedFlags),
		weights:       cfg.MovieWeights,
	}
	e.blacklist = e.loadBlacklist()
	return &MovieSearcher{engine: e}
}

func (s *MovieSearcher) Search(ctx context.Context, imdb, title string, year int) (SearchResult, error) {
	streams, search, err := s.engine.getMovieStreams(ctx, imdb, title, year)
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Complete: search.Complete, HadRaw: search.HadRaw, Candidates: []Candidate{}}
	for _, m := range streams {
		out.Candidates = append(out.Candidates, Candidate{
			Hash: m.Hash, Name: firstLine(m.Title), ReleaseTitle: m.Title, DownloadURL: m.DownloadURL,
			SizeGB: m.SizeGB, Seeders: m.Seeders, Is4K: m.Is4K, Score: m.QualityScore, QualityScore: m.QualityScore,
		})
	}
	finishCandidates(ctx, s.engine.prowlarr, &out, func(h string) bool {
		_, banned := s.engine.blacklist.Hashes[h]
		return banned
	}, s.engine.logger.Printf)
	return out, nil
}

// ShowSearcher runs getStreams the way processShow does, without the run's gates.
type ShowSearcher struct{ engine *TVGoEngine }

func NewShowSearcher(cfg SearchConfig) *ShowSearcher {
	e := &TVGoEngine{
		tmdb:          tmdb.NewClient(cfg.TMDBAPIKey),
		torrentio:     torrentio.NewClient(cfg.TorrentioURL, torrentioOptions),
		prowlarr:      newProwlarrClient(cfg.ProwlarrCfg),
		limiter:       rate.NewLimiter(rate.Every(500*time.Millisecond), 1),
		logger:        searchLogger(cfg),
		knownTitles:   make(map[int][]string),
		blacklistFile: filepath.Join(cfg.StateDir, "blacklist.json"),
		reITA:         CompileLanguageRegex(cfg.Language.PreferredTerms, cfg.Language.PreferredFlags),
		reExclLang:    CompileLanguageRegex(ExcludedTitleTerms(cfg.Language.ExcludedFlags), cfg.Language.ExcludedFlags),
		exclLanguages: ExcludedLanguageSet(cfg.Language.ExcludedFlags),
		weights:       cfg.TVWeights,
		tmdbLangs:     TMDBEpisodeLanguages(cfg.Language.PreferredFlags),
	}
	e.blacklist = e.loadBlacklist()
	return &ShowSearcher{engine: e}
}

// KnownTitles fetches the show's details and every name its releases may carry.
func (s *ShowSearcher) KnownTitles(ctx context.Context, tmdbID int) (*tmdb.TVDetail, []string, error) {
	details, err := s.engine.tmdb.TVDetails(ctx, tmdbID)
	if err != nil {
		return nil, nil, fmt.Errorf("tmdb details for %d: %w", tmdbID, err)
	}
	return details, s.engine.showKnownTitles(ctx, tmdbID, details), nil
}

func (s *ShowSearcher) Search(ctx context.Context, imdb string, tmdbID int, seasons ...int) (SearchResult, error) {
	details, known, err := s.KnownTitles(ctx, tmdbID)
	if err != nil {
		return SearchResult{}, err
	}
	name := details.Name
	if name == "" {
		name = details.OriginalName
	}
	streams, searched := s.engine.getStreams(ctx, imdb, tmdbID, name, details, seasons...)
	sort.SliceStable(streams, func(i, j int) bool { return streams[i].Priority > streams[j].Priority })

	out := SearchResult{
		Candidates: []Candidate{}, Seasons: map[int]SeasonStatus{}, Complete: len(searched) > 0,
		ShowName: name, FirstAirDate: details.FirstAirDate, KnownTitles: known,
	}
	for season, st := range searched {
		out.Seasons[season] = SeasonStatus{Complete: st.Complete, RawSeen: st.RawSeen}
		out.Complete = out.Complete && st.Complete
		out.HadRaw = out.HadRaw || st.RawSeen
	}
	for _, t := range streams {
		out.Candidates = append(out.Candidates, Candidate{
			Hash: t.Hash, Name: firstLine(t.Title), ReleaseTitle: t.Title, DownloadURL: t.DownloadURL,
			SizeGB: t.SizeGB, Seeders: t.Seeders, Is4K: reTV4K.MatchString(t.Title),
			Season: t.Season, Episode: t.EpisodeNum, Score: t.Priority, QualityScore: t.QualityScore,
		})
	}
	finishCandidates(ctx, s.engine.prowlarr, &out, func(h string) bool {
		return s.engine.isHashBlacklisted(h) || s.engine.isDeadPackHash(h)
	}, s.engine.logger.Printf)
	return out, nil
}

// ShowMatcher is the add hook: the torrent's video files must belong to the show.
func (s *ShowSearcher) ShowMatcher(showName string, knownTitles []string, releaseTitle string) func([]string) error {
	return func(paths []string) error {
		var files []FileStat
		for _, p := range paths {
			if library.IsVideoFile(p) {
				files = append(files, FileStat{Path: p})
			}
		}
		if len(files) == 0 || s.engine.contentsBelongToShow(files, knownTitles, showName, releaseTitle) {
			return nil
		}
		return fmt.Errorf("the torrent's files do not belong to %s", showName)
	}
}

// Overrides has the shape of config.json's quality_scoring and language blocks.
type Overrides struct {
	Movies   json.RawMessage `json:"movies,omitempty"`
	TV       json.RawMessage `json:"tv,omitempty"`
	Language json.RawMessage `json:"language,omitempty"`
}

// ApplyOverrides merges raw onto base for one search; nothing is written back.
func ApplyOverrides(base SearchConfig, raw json.RawMessage) (SearchConfig, *Overrides, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return base, nil, nil
	}
	var o Overrides
	if err := strictDecode(trimmed, &o); err != nil {
		return base, nil, fmt.Errorf("overrides: %w", err)
	}
	if err := mergeInto(o.Movies, &base.MovieWeights); err != nil {
		return base, nil, fmt.Errorf("overrides.movies: %w", err)
	}
	if err := mergeInto(o.TV, &base.TVWeights); err != nil {
		return base, nil, fmt.Errorf("overrides.tv: %w", err)
	}
	// base shares its slices with the live config, and decoding into a slice reuses its
	// backing array: copy them, or a one-search override rewrites the sync's languages.
	base.Language.PreferredTerms = slices.Clone(base.Language.PreferredTerms)
	base.Language.PreferredFlags = slices.Clone(base.Language.PreferredFlags)
	base.Language.ExcludedFlags = slices.Clone(base.Language.ExcludedFlags)
	if err := mergeInto(o.Language, &base.Language); err != nil {
		return base, nil, fmt.Errorf("overrides.language: %w", err)
	}
	return base, &o, nil
}

// mergeInto decodes onto a filled struct, so fields raw does not name keep their value.
func mergeInto(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return strictDecode(raw, dst)
}

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
