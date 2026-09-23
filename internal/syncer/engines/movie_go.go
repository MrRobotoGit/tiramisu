package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog/mediaserver"
	"tiramisu/internal/catalog/rottentomatoes"
	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/catalog/torrentio"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
	"tiramisu/internal/prowlarr"
)

// MovieGoEngine is the pure Go implementation of movie sync.
type MovieGoEngine struct {
	gostorm   *GoStormClient
	tmdb      *tmdb.Client
	torrentio *torrentio.Client
	prowlarr  *prowlarr.Client
	rt        *rottentomatoes.Client
	plexURL   string
	plexToken string
	plexLib   int
	mediasrv  mediaserver.Client
	moviesDir string
	stateDir  string
	limiter   *rate.Limiter
	logger    *log.Logger

	// Negative caches
	noMKVCache     map[string]CacheEntry
	noStreamsCache map[string]CacheEntry
	recheckCache   map[string]CacheEntry
	addFailCache   map[string]CacheEntry
	imdbCache      map[string]IMDBCacheEntry
	noMKVCFile     string
	noStreamsCFile string
	recheckCFile   string
	addFailCFile   string
	imdbCFile      string

	blacklist     BlacklistData
	blacklistFile string

	db       *metadb.DB
	audioReg library.AudioRegistry
	// deadTitles holds the IMDB ids whose current release stopped resolving its
	// metadata. They are re-searched this run, and dropped only if nothing live
	// turns up.
	deadTitles map[string]bool

	invalidatePath func(string)

	reITA         *regexp.Regexp
	reExclLang    *regexp.Regexp
	exclLanguages map[string]bool

	weights config.MovieWeights
}

// CacheEntry is a generic cache entry with timestamp.
type CacheEntry struct {
	Reason string `json:"reason,omitempty"`
	Title  string `json:"title,omitempty"`
	TS     int64  `json:"ts"`
}

// IMDBCacheEntry caches TMDB→IMDB mapping.
type IMDBCacheEntry struct {
	IMDBID string `json:"imdb_id"`
	Title  string `json:"title"`
	TS     int64  `json:"ts"`
}

// BlacklistData holds blocked hashes and titles.
type BlacklistData struct {
	Hashes map[string]string `json:"hashes,omitempty"`
	Titles []string          `json:"titles,omitempty"`
}

// MovieEngineConfig holds config for the movie engine.
type MovieEngineConfig struct {
	GoStormURL      string
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	MediaServerType string
	PlexLib         int
	MoviesDir       string
	StateDir        string
	LogsDir         string
	ProwlarrCfg     prowlarr.ConfigProwlarr
	DB              *metadb.DB
	// AudioRegistry lets the host say "audio is configured but its registry is
	// unavailable", which a nil DB cannot express and must not be read as "no audio".
	AudioRegistry library.AudioRegistry
	Language      config.LanguageConfig
	Weights       config.MovieWeights
	// InvalidatePath, when set, is called after removing a stub file so the FUSE
	// layer drops its cached state for it (see main.invalidateSyncRemovedPath).
	InvalidatePath func(string)
}

// mMovieMetadataWait/mMovie4KMetadataWait are how long a candidate is given to
// produce its metadata. Vars, not consts, so a test can exercise the timeout path
// without waiting on the wall clock.
var (
	mMovieMetadataWait   = 12
	mMovie4KMetadataWait = 45
)

// Movie thresholds
const (
	mMovieUpgradePct   = 1.1
	mMovieProcessSleep = 1 * time.Second
	noMKVCacheTTL      = 12 * time.Hour
	// deadReleaseFailures/deadReleaseSpan: what tells a dead swarm from a bad
	// evening is failures spread over time, not how many arrived together.
	deadReleaseFailures = 3
	deadReleaseSpan     = 24 * time.Hour
	noStreamsCacheTTL   = 24 * time.Hour
	recheckCacheTTL     = 48 * time.Hour
	recheck1080pTTL     = 6 * time.Hour
	recheckNoFileTTL    = 24 * time.Hour
	addFailCacheTTL     = 168 * time.Hour
)

var (
	reM4K    = regexp.MustCompile(`(?i)2160p|4[kK]|uhd`)
	reM1080p = regexp.MustCompile(`(?i)1080p|1080i|fhd`)
	reM720p  = regexp.MustCompile(`(?i)720p|720i`)
	// \b treats "_" as a word char, so "\bhdr\b" misses "_HDR_" - use a custom boundary.
	reMHDR    = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])hdr(?:$|[^A-Za-z0-9])|hdr10\+?`)
	reMDV     = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])dv(?:$|[^A-Za-z0-9])|dovi|dolby.?vision`)
	reMAtmos  = regexp.MustCompile(`(?i)atmos`)
	reM51     = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	reMStereo = regexp.MustCompile(`(?i)stereo|aac|mp3|2\.0`)
	// Same rule as library.BuildMovieFilename: no separator required before the word,
	// so "BDRemux" and "UHDRemux" count as the remux they are.
	reMRemux   = regexp.MustCompile(`(?i)remux(?:$|[^A-Za-z0-9])`)
	reMGarbage = regexp.MustCompile(`(?i)camrip|hdcam|hdts|telesync|\bts\b|telecine|\btc\b|\bscr\b|screener|webscreener`)
	reMSeeders = regexp.MustCompile(`👤\s*(\d+)`)
	// reQualityMarker: where a stub filename stops being the title and starts being
	// release metadata. No audio-channel alternative (5.1, 7.1): titleFromStubName
	// splits on "." before matching, so such a token never reaches here whole.
	reQualityMarker = regexp.MustCompile(`(?i)^(2160p|1080p|720p|480p|4k|uhd|hdr|dv|atmos|remux|bluray|web|webrip|web-dl|hevc|x264|x265|multi|ita|eng)$`)
	reMHashURL      = regexp.MustCompile(`(?i)link=([a-f0-9]{40})`)
	reMMKVHash8     = regexp.MustCompile(`(?i)_([a-f0-9]{8})\.mkv$`)
	reMYear         = regexp.MustCompile(`[._]((?:19|20)\d{2})[._]`)
	reMNonWord      = regexp.MustCompile(`[^a-z0-9]`)
	reMQuality      = regexp.MustCompile(`(?i)(2160p|1080p|720p|4k|uhd)`)
)

// NewMovieGoEngine creates a new Go movie sync engine.
func NewMovieGoEngine(cfg MovieEngineConfig) *MovieGoEngine {
	var prowlarrClient *prowlarr.Client
	if cfg.ProwlarrCfg.Enabled {
		prowlarrClient = prowlarr.NewClient(cfg.ProwlarrCfg)
	}

	logPath := filepath.Join(cfg.LogsDir, "movies-sync.log")
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "[MovieSync] ", log.LstdFlags)

	e := &MovieGoEngine{
		gostorm:   NewGoStormClient(cfg.GoStormURL),
		tmdb:      tmdb.NewClient(cfg.TMDBAPIKey),
		torrentio: torrentio.NewClient(cfg.TorrentioURL, "sort=qualitysize|qualityfilter=480p,720p,scr,cam"),
		prowlarr:  prowlarrClient,
		rt:        rottentomatoes.NewClient(),
		plexURL:   cfg.PlexURL,
		plexToken: cfg.PlexToken,
		plexLib:   cfg.PlexLib,
		mediasrv:  mediaserver.New(cfg.MediaServerType, cfg.PlexURL, cfg.PlexToken),
		moviesDir: cfg.MoviesDir,
		stateDir:  cfg.StateDir,
		limiter:   rate.NewLimiter(rate.Every(250*time.Millisecond), 1),
		logger:    logger,

		noMKVCFile:     filepath.Join(cfg.StateDir, "no_mkv_hashes.json"),
		noStreamsCFile: filepath.Join(cfg.StateDir, "movie_no_streams_cache.json"),
		recheckCFile:   filepath.Join(cfg.StateDir, "movie_recheck_cache.json"),
		addFailCFile:   filepath.Join(cfg.StateDir, "movie_add_fail_cache.json"),
		imdbCFile:      filepath.Join(cfg.StateDir, "movie_imdb_cache.json"),
		blacklistFile:  filepath.Join(cfg.StateDir, "blacklist.json"),
		invalidatePath: cfg.InvalidatePath,

		reITA:         CompileLanguageRegex(cfg.Language.PreferredTerms, cfg.Language.PreferredFlags),
		reExclLang:    CompileLanguageRegex(ExcludedTitleTerms(cfg.Language.ExcludedFlags), cfg.Language.ExcludedFlags),
		exclLanguages: ExcludedLanguageSet(cfg.Language.ExcludedFlags),

		weights: cfg.Weights,
	}

	e.db = cfg.DB
	e.audioReg = cfg.AudioRegistry
	e.noMKVCache = e.loadNoMKVCache()
	e.noStreamsCache = e.loadCache(e.noStreamsCFile)
	e.recheckCache = e.loadCache(e.recheckCFile)
	e.addFailCache = e.loadCache(e.addFailCFile)
	e.imdbCache = e.loadIMDBCache(e.imdbCFile)
	e.blacklist = e.loadBlacklist()
	e.deadTitles = make(map[string]bool)

	e.pruneExpiredCaches()

	return e
}

// removeStub deletes a stub file, invalidates its FUSE cache state, and removes the
// underlying torrent from GoStorm. hash may be empty; a RemoveTorrent error doesn't
// block the stub deletion.
func (e *MovieGoEngine) removeStub(ctx context.Context, path, hash string) {
	if hash != "" {
		if _, err := e.dropTorrent(ctx, hash); err != nil {
			e.logger.Printf("[MovieSync] WARNING: failed to remove torrent %s for %s: %v", hash, filepath.Base(path), err)
		}
		// The failure counter outlives the release otherwise: if this hash is ever
		// selected again, a stale count already past the threshold would condemn it
		// before it has had a chance to fail.
		if e.db != nil {
			if err := e.db.ClearMetadataFailure(hash); err != nil {
				e.logger.Printf("[MovieSync] WARNING: failed to clear metadata failures for %s: %v", hash, err)
			}
		}
	}
	os.Remove(path)
	if e.invalidatePath != nil {
		e.invalidatePath(path)
	}
}

// flagDeadTitles marks the titles whose release stopped answering with its
// metadata, so this run re-searches them instead of skipping them as already
// present. The engine counts the failures; the threshold lives here because
// what to do with a dead release is a library decision, not an engine one.
//
// The failing hash goes into noMKVCache, which the candidate loop already
// consults: that keeps the dead release out of this run's selection without a
// second writer for blacklist.json, which the mount's unlink handler owns.
func (e *MovieGoEngine) flagDeadTitles(existingIndex map[string]movieFile) {
	// Rebuilt every run, before the DB guard: a flag is a decision about this pass,
	// and carrying it over would let a later run act on a title whose release has
	// since been replaced.
	e.deadTitles = make(map[string]bool)

	if e.db == nil {
		return
	}
	dead, err := e.db.MetadataFailuresOver(deadReleaseFailures, deadReleaseSpan)
	if err != nil {
		e.logger.Printf("[MovieSync] WARNING: could not read metadata failures: %v", err)
		return
	}
	if len(dead) == 0 {
		return
	}
	deadSet := make(map[string]bool, len(dead))
	for _, h := range dead {
		deadSet[strings.ToLower(h)] = true
	}
	for imdbID, mf := range existingIndex {
		if mf.hash == "" || !deadSet[strings.ToLower(mf.hash)] {
			continue
		}
		e.deadTitles[imdbID] = true
		e.setCache(e.noMKVCache, mf.hash, CacheEntry{Reason: "dead_swarm", TS: time.Now().Unix()})
		// Every negative cache has to go, not just the recheck one: a title that hit
		// no_streams (24h) or add_failed (168h) would log "re-searching" and then return
		// from evaluateTitle without searching anything, for up to a week.
		delete(e.recheckCache, imdbID)
		delete(e.noStreamsCache, imdbID)
		delete(e.addFailCache, imdbID)
		e.logger.Printf("[MovieSync] Dead release for %s (%s): re-searching", filepath.Base(mf.path), mf.hash[:8])
	}
}

// dropDeadStub removes the stub of a title flagged dead once this run has
// established there is nothing live to replace it with. A search that failed to
// complete never gets here: it says nothing about the title.
func (e *MovieGoEngine) dropDeadStub(ctx context.Context, imdbID string, existing movieFile, conclusive, rawSeen bool) bool {
	if !e.deadTitles[imdbID] || existing.path == "" {
		return false
	}
	// Only a run that actually reached the swarm may condemn a title. An aborted
	// run, or one where candidates failed to be added at all, says nothing: without
	// this an indexer outage would empty the library in a single pass.
	if !conclusive || ctx.Err() != nil {
		e.logger.Printf("[MovieSync] Dead release for %s: search was inconclusive, keeping the stub", filepath.Base(existing.path))
		return false
	}
	e.logger.Printf("[MovieSync] No live release for %s (raw releases seen: %t): removing dead stub", filepath.Base(existing.path), rawSeen)
	e.removeStub(ctx, existing.path, existing.hash)
	delete(e.deadTitles, imdbID)
	return true
}

// reapFlaggedTitles gives the titles still flagged at the end of a run their own
// pass. Discovery only returns the last six months plus what is trending, so an
// aging library would otherwise collect flags that are never acted on: neither
// re-searched nor removed.
func (e *MovieGoEngine) reapFlaggedTitles(ctx context.Context, existingIndex map[string]movieFile, diskHashes map[string]bool) {
	if len(e.deadTitles) == 0 {
		return
	}
	pending := make([]string, 0, len(e.deadTitles))
	for imdbID := range e.deadTitles {
		pending = append(pending, imdbID)
	}
	sort.Strings(pending) // deterministic order, so a run is reproducible from the log
	for _, imdbID := range pending {
		select {
		case <-ctx.Done():
			return
		default:
		}
		existing := existingIndex[imdbID]
		if existing.path == "" {
			continue
		}
		title, year := titleFromStubName(filepath.Base(existing.path))
		if title == "" {
			continue
		}
		// An unknown year must stay empty: "0" is not a date, and BuildMovieFilename would
		// drop it and rename the stub, which Plex reads as a different file.
		releaseDate := ""
		if year > 0 {
			releaseDate = strconv.Itoa(year)
		}
		e.logger.Printf("[MovieSync] Dead release outside discovery: evaluating %s (%s)", title, imdbID)
		e.evaluateTitle(ctx, imdbID, title, releaseDate, year, existing, diskHashes)
		time.Sleep(mMovieProcessSleep)
	}
}

// titleFromStubName recovers a searchable title and year from a stub filename. The
// name is built from the title plus quality markers and an 8-char hash, so cutting
// at the first marker is enough; the IMDB id carries the search either way, and this
// only feeds the secondary title query.
func titleFromStubName(name string) (string, int) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if i := strings.LastIndex(base, "_"); i > 0 && len(base)-i == 9 {
		base = base[:i] // drop the trailing _hash8
	}
	sep := func(r rune) bool { return r == '_' || r == '.' || r == ' ' }
	parts := strings.FieldsFunc(base, sep)
	var words []string
	for _, p := range parts {
		if reQualityMarker.MatchString(p) {
			break // everything past the first marker is release metadata
		}
		words = append(words, p)
	}
	// The release year is the LAST year-like token, not the first: a title can be a year
	// ("1917", "2012") or end with one ("Blade Runner 2049"), and taking the first would
	// steal it from the title and report the wrong year. Index 0 is never the year, or a
	// title that is just a year would come back empty and never be reaped.
	year := 0
	for i := len(words) - 1; i > 0; i-- {
		if n, err := strconv.Atoi(words[i]); err == nil && n >= 1900 && n <= 2100 {
			year = n
			words = words[:i]
			break
		}
	}
	return strings.Join(words, " "), year
}

func (e *MovieGoEngine) Name() string { return "movies" }

func (e *MovieGoEngine) Run(ctx context.Context) error {
	// Deferred so an early return still persists what the run learned about dead
	// candidates. The nominal path saves before the closing phases too: this is the
	// backstop, not the only write.
	defer e.saveAllCaches()

	e.logger.Printf("[MovieSync] Starting discovery...")
	movies, err := e.discoverMovies(ctx)
	if err != nil {
		return fmt.Errorf("discover movies: %w", err)
	}
	e.logger.Printf("[MovieSync] Discovered %d movies", len(movies))

	existingIndex, diskHashes := e.buildExistingMovieIndex()
	e.logger.Printf("[MovieSync] Existing index: %d movies, %d hashes on disk", len(existingIndex), len(diskHashes))

	e.flagDeadTitles(existingIndex)

	created := 0
	for i, m := range movies {
		select {
		case <-ctx.Done():
			e.logger.Printf("[MovieSync] Stopped after %d/%d movies (%d created)", i, len(movies), created)
			return ctx.Err()
		default:
		}

		if e.processMovie(ctx, m, existingIndex, diskHashes) {
			created++
		}
		time.Sleep(mMovieProcessSleep)
	}

	e.reapFlaggedTitles(ctx, existingIndex, diskHashes)

	e.logger.Printf("[MovieSync] Processing complete: %d created out of %d discovered", created, len(movies))
	// Saved here, and again by the deferred call: what the run learned is on disk
	// before the long rehydrate/cleanup phases, which a SIGKILL would cut through.
	e.saveAllCaches()
	e.rehydrateMissingTorrents(ctx)
	e.cleanupOrphanedFiles(ctx)

	// Plex skips this without a section ID; Jellyfin refreshes every library and ignores it.
	if err := e.mediasrv.RefreshLibrary(context.Background(), e.plexLib); err != nil {
		e.logger.Printf("[MovieSync] Warning: media server library refresh failed: %v", err)
	}

	return nil
}

func (e *MovieGoEngine) discoverMovies(ctx context.Context) ([]tmdb.Movie, error) {
	cutoff := time.Now().AddDate(0, -6, 0).Format("2006-01-02")
	currentYear := time.Now().Year() + 1
	dateLTE := fmt.Sprintf("%d-12-31", currentYear)

	var all []tmdb.Movie
	seen := make(map[int]bool)

	endpoints := []func(context.Context) ([]tmdb.Movie, error){
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMovies(ctx, "en", cutoff, dateLTE, 12)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMovies(ctx, "it", cutoff, dateLTE, 3)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/now_playing", "US", 1)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/now_playing", "GB", 1)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/popular", "US", 3)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.TrendingMovies(ctx, 1)
		},
	}

	for _, fn := range endpoints {
		movies, err := fn(ctx)
		if err != nil {
			continue
		}
		for _, m := range movies {
			if !seen[m.ID] && !e.exclLanguages[m.Language] {
				seen[m.ID] = true
				all = append(all, m)
			}
		}
	}

	// Rotten Tomatoes "Movies at Home": recent digital/streaming releases. Titles overlap only
	// ~20% with the TMDB endpoints above (checked 2026-07-13), so each one is resolved against
	// TMDB by title+year to fold into the same dedup/filter path as everything else.
	if e.rt != nil {
		if rtMovies, err := e.rt.FetchMoviesAtHome(ctx); err == nil {
			for _, rm := range rtMovies {
				m, err := e.tmdb.SearchMovieBest(ctx, rm.Title, rm.Year)
				if err != nil {
					continue
				}
				if !seen[m.ID] && !e.exclLanguages[m.Language] {
					seen[m.ID] = true
					all = append(all, m)
				}
			}
		}
	}

	return all, nil
}

type movieFile struct {
	path  string
	imdb  string
	hash  string
	score int
	// is4K is stored, not inferred from score: the 4K weight is user-configurable.
	is4K bool
}

func (e *MovieGoEngine) buildExistingMovieIndex() (map[string]movieFile, map[string]bool) {
	index := make(map[string]movieFile)
	diskHashes := make(map[string]bool)
	if _, err := os.Stat(e.moviesDir); err != nil {
		return index, diskHashes
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		// Collect hash8 from filename (last 8 hex chars before .mkv)
		if m := reMMKVHash8.FindStringSubmatch(info.Name()); len(m) >= 2 {
			diskHashes[strings.ToLower(m[1])] = true
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 10240 {
			return nil
		}

		var imdb string
		content := strings.TrimSpace(string(data))

		var url string
		// Try JSON format first (new Go format)
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(content), &obj); err == nil {
				imdb, _ = obj["imdb"].(string)
				url, _ = obj["url"].(string)
			}
		} else {
			// Text format (old Python format): line 1 = URL, line 4 = IMDB ID
			lines := strings.SplitN(content, "\n", 4)
			if len(lines) >= 1 {
				url = strings.TrimSpace(lines[0])
			}
			if len(lines) >= 4 {
				imdb = strings.TrimSpace(lines[3])
			}
		}

		if imdb == "" {
			return nil
		}
		var hash string
		if m := reMHashURL.FindStringSubmatch(url); len(m) >= 2 {
			hash = strings.ToLower(m[1])
		}
		is4K := reM4K.MatchString(info.Name())
		score := e.calculateMovieScore(info.Name(), 0, 0, is4K, e.weights)
		if existing, ok := index[imdb]; !ok || score > existing.score {
			index[imdb] = movieFile{path: path, imdb: imdb, hash: hash, score: score, is4K: is4K}
		}
		return nil
	})

	return index, diskHashes
}

func (e *MovieGoEngine) processMovie(ctx context.Context, movie tmdb.Movie, existingIndex map[string]movieFile, diskHashes map[string]bool) bool {
	title := movie.Title
	if title == "" {
		title = movie.OriginalTitle
	}
	if title == "" {
		return false
	}

	// Blacklist check
	if e.isBlacklisted(title) {
		return false
	}

	// Resolve IMDB
	imdbID := e.resolveIMDB(ctx, movie.ID, title)
	if imdbID == "" {
		return false
	}

	year := 0
	if len(movie.ReleaseDate) >= 4 {
		year, _ = strconv.Atoi(movie.ReleaseDate[:4])
	}
	return e.evaluateTitle(ctx, imdbID, title, movie.ReleaseDate, year, existingIndex[imdbID], diskHashes)
}

// candidateBlocked reports what a candidate that fails the quality bar means for the
// title. The bar is the existing score plus the upgrade margin, and it is the user's: a
// dead stub never gets to win it, but it is never replaced by a downgrade either, it is
// dropped after the run. blocked alone (live stub) just parks the title.
func candidateBlocked(stubDead bool, candidateScore, existingScore int) (blocked, dropDead bool) {
	if float64(candidateScore) > float64(existingScore)*mMovieUpgradePct {
		return false, false
	}
	return true, stubDead
}

// evaluateTitle runs the candidate search for one title and acts on the result. It is
// separate from processMovie so a title the discovery feed never returns - anything
// older than the six-month window - can still be reached, which is the only way the
// reaper sees an aging library.
func (e *MovieGoEngine) evaluateTitle(ctx context.Context, imdbID, title, releaseDate string, year int, existing movieFile, diskHashes map[string]bool) bool {
	// TTL recheck upgrade-aware: 1080p esistente → 6h (cerca upgrade 4K),
	// 4K esistente → 48h, nessun file → 24h.
	recheckTTL := recheckNoFileTTL
	if existing.path != "" {
		if existing.is4K {
			recheckTTL = recheckCacheTTL
		} else {
			recheckTTL = recheck1080pTTL
		}
	}

	// Check negative caches. A title flagged dead this run skips them: its stub is
	// unplayable, so a cached "nothing better" from a healthy release no longer applies,
	// and honouring it would make the reaper log a search it never ran.
	if !e.deadTitles[imdbID] {
		if e.isInCache(e.noStreamsCache, imdbID, noStreamsCacheTTL) {
			return false
		}
		if e.isInCache(e.recheckCache, imdbID, recheckTTL) {
			return false
		}
		if e.isInCache(e.addFailCache, imdbID, addFailCacheTTL) {
			return false
		}
	}

	// Get streams
	e.logger.Printf("[MovieSync] Processing: %s (%s)", title, imdbID)
	candidates, search, err := e.getMovieStreams(ctx, imdbID, title, year)
	if err != nil {
		// A search that never completed says nothing about the title. Caching it would
		// hide the film for a day over a transient indexer timeout.
		e.logger.Printf("[MovieSync] Search failed for %s, not caching: %v", title, err)
		return false
	}
	if len(candidates) == 0 {
		// Completeness is what matters, not whether releases came back: every indexer
		// answering with nothing usable is a statement about the title, while one
		// indexer down is a statement about the search.
		e.dropDeadStub(ctx, imdbID, existing, search.Complete, search.HadRaw)
		if search.HadRaw {
			e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_valid_stream", TS: time.Now().Unix()})
		} else {
			e.setCache(e.noStreamsCache, imdbID, CacheEntry{Title: title, TS: time.Now().Unix()})
		}
		return false
	}
	delete(e.noStreamsCache, imdbID)

	// Check if we already have this movie
	existingPath := existing.path
	existingScore := existing.score

	// addFailed marks candidates the engine could not even take: their silence is
	// about us, not about the swarm, so the title must not be condemned for it.
	addFailed := false

	// Try candidates
	for _, c := range candidates {
		if existingPath != "" {
			blocked, dropDead := candidateBlocked(e.deadTitles[imdbID], c.QualityScore, existingScore)
			if dropDead {
				// The stub cannot play, but the bar still applies: no candidate clears
				// it, so the post-loop drop removes the title instead of downgrading it.
				continue
			}
			if blocked {
				e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_better_stream", TS: time.Now().Unix()})
				return false
			}
		}

		if e.isInCache(e.noMKVCache, c.Hash, noMKVCacheTTL) {
			continue
		}

		if diskHashes[strings.ToLower(c.Hash[len(c.Hash)-8:])] {
			continue
		}

		magnet := releaseMagnet(ctx, e.prowlarr, e.gostorm, c.Hash, title, c.DownloadURL, e.logger.Printf)
		hash, confirmed, err := e.gostorm.AddTorrentConfirmed(ctx, magnet, title)
		if err != nil || hash == "" || !confirmed {
			// The engine refused the release, or answered without acknowledging it:
			// either way nothing was learned about the swarm.
			addFailed = true
			e.setCache(e.addFailCache, imdbID, CacheEntry{Title: title, Reason: "add_failed", TS: time.Now().Unix()})
			continue
		}
		delete(e.addFailCache, imdbID)

		maxWait := mMovieMetadataWait
		if c.Is4K {
			maxWait = mMovie4KMetadataWait
		}

		info, err := e.gostorm.GetTorrentInfo(ctx, hash, maxWait)
		if err != nil {
			e.setCache(e.noMKVCache, hash, CacheEntry{Reason: "metadata_timeout", TS: time.Now().Unix()})
			_, _ = e.dropTorrent(ctx, hash)
			continue
		}

		videoFiles := e.filterVideoFiles(info.FileStats, c.Is4K)
		if len(videoFiles) == 0 {
			e.setCache(e.noMKVCache, hash, CacheEntry{Reason: "no_valid_files", TS: time.Now().Unix()})
			_, _ = e.dropTorrent(ctx, hash)
			continue
		}

		// Take largest
		sort.Slice(videoFiles, func(i, j int) bool {
			return videoFiles[i].Length > videoFiles[j].Length
		})
		bestFile := videoFiles[0]

		// Remove existing if upgrading
		if existingPath != "" {
			e.logger.Printf("[MovieSync] Upgrade: removing %s", filepath.Base(existingPath))
			e.removeStub(ctx, existingPath, existing.hash)
		}

		filename := e.buildMovieFilename(title, releaseDate, c)
		mkvPath := filepath.Join(e.moviesDir, filename)
		streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", e.gostorm.baseURL, hash, bestFile.ID)

		if e.createMKV(mkvPath, streamURL, bestFile.Length, magnet, imdbID) {
			res := "4K"
			if !c.Is4K {
				res = "1080p"
			}
			// The title has a live release again: the flag must not survive, or a later
			// run with no candidates would drop the replacement we just created.
			delete(e.deadTitles, imdbID)
			// The index was built before the run: without this the reap pass, which
			// runs at the end with the same map, cannot see what the loop just wrote
			// and would add the very same release a second time.
			if len(hash) >= 8 {
				diskHashes[strings.ToLower(hash[len(hash)-8:])] = true
			}
			e.logger.Printf("[MovieSync] Created: %s (%s, %.1fGB, score:%d)", filename, res, float64(bestFile.Length)/1024/1024/1024, c.QualityScore)
			e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "processed", TS: time.Now().Unix()})
			return true
		}

		_, _ = e.dropTorrent(ctx, hash)
	}

	// Every candidate was exhausted without a live one: the stub points at a swarm
	// that cannot serve it. A live candidate would have returned above through the
	// ordinary upgrade path.
	e.dropDeadStub(ctx, imdbID, existing, !addFailed && search.Complete, search.HadRaw)

	e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_better_stream", TS: time.Now().Unix()})
	return false
}

type MovieStream struct {
	Title        string
	Hash         string
	Is4K         bool
	QualityScore int
	Seeders      int
	SizeGB       float64
	DownloadURL  string // the indexer's link, for the .torrent of the release picked
}

// getMovieStreams asks both indexers and scores their releases together. Stopping at the
// first source that yields anything hid better releases: the two carry different catalogues,
// and whichever answered first won regardless of quality.
//
// The error return means no search completed. It must stay distinct from "searched and found
// nothing", because the caller caches the latter for a day.
// streamSearch says what a search run actually established. HadRaw means releases
// came back at all; Complete means every configured indexer answered. Condemning a
// title needs Complete: with one indexer down, "no valid release" is a statement
// about the search, not about the title.
type streamSearch struct {
	HadRaw   bool
	Complete bool
}

func (e *MovieGoEngine) getMovieStreams(ctx context.Context, imdbID, title string, year int) ([]MovieStream, streamSearch, error) {
	var streams []prowlarr.Stream
	prowlarrOK, torrentioOK := false, false

	if e.prowlarr != nil {
		// Gate before the hashes are resolved: each resolution is a serialised round
		// trip through Prowlarr, and the gates below discard most candidates anyway.
		// blacklist_hash cannot run yet (the hash is what we are avoiding fetching),
		// so a blacklisted release survives here and is rejected by the same gates a
		// moment later, in filterMovieStreams.
		keep := func(s prowlarr.Stream) bool {
			ms, _ := e.classifyMovieStream(s)
			return ms != nil
		}
		ps, complete, err := e.prowlarr.FetchTorrentsStatus(imdbID, "movie", title, year, keep)
		if err != nil {
			e.logger.Printf("[MovieSync] Prowlarr search failed: %v", err)
		} else {
			// Partial answers still feed the candidate list, but they cannot make the
			// search complete: some of Prowlarr's own queries never ran.
			if !complete {
				e.logger.Printf("[MovieSync] Prowlarr answered partially for %s: search is not conclusive", imdbID)
			}
			prowlarrOK = complete
			streams = append(streams, ps...)
		}
	} else {
		prowlarrOK = true // not configured: nothing to fail
	}

	tioStreams, err := e.torrentio.FetchMovieStreams(ctx, imdbID)
	if err != nil {
		e.logger.Printf("[MovieSync] Torrentio search failed: %v", err)
	} else {
		torrentioOK = true
		for _, s := range tioStreams {
			streams = append(streams, prowlarr.Stream{
				Name:     s.Name,
				Title:    s.Title,
				InfoHash: s.InfoHash,
				SizeGB:   float64(s.Size) / (1024 * 1024 * 1024),
			})
		}
	}

	if !prowlarrOK && !torrentioOK {
		return nil, streamSearch{}, fmt.Errorf("every indexer search failed for %s", imdbID)
	}

	streams = dedupStreamsByHash(streams)
	return e.filterMovieStreams(streams), streamSearch{
		HadRaw:   len(streams) > 0,
		Complete: prowlarrOK && torrentioOK,
	}, nil
}

// dedupStreamsByHash keeps the first occurrence of each infohash: the two indexers list the
// same releases, and a duplicate would be scored and attempted twice.
func dedupStreamsByHash(in []prowlarr.Stream) []prowlarr.Stream {
	seen := make(map[string]struct{}, len(in))
	out := make([]prowlarr.Stream, 0, len(in))
	for _, s := range in {
		k := strings.ToLower(s.InfoHash)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (e *MovieGoEngine) filterMovieStreams(streams []prowlarr.Stream) []MovieStream {
	rejectCounts := map[string]int{}
	var passed []MovieStream
	n4K := 0
	for _, s := range streams {
		c, reason := e.classifyMovieStream(s)
		if c == nil {
			rejectCounts[reason]++
			continue
		}
		if c.Is4K {
			n4K++
		}
		passed = append(passed, *c)
	}

	if len(streams) > 0 {
		e.logger.Printf("[filter] streams=%d 4K=%d 1080p=%d rejected=%v",
			len(streams), n4K, len(passed)-n4K, rejectCounts)
	}

	// QualityScore decides; 4K only breaks ties, so custom weights stay in control.
	sort.SliceStable(passed, func(i, j int) bool {
		if passed[i].QualityScore != passed[j].QualityScore {
			return passed[i].QualityScore > passed[j].QualityScore
		}
		return passed[i].Is4K && !passed[j].Is4K
	})
	return passed
}

func (e *MovieGoEngine) classifyMovieStream(s prowlarr.Stream) (*MovieStream, string) {
	title := s.Title
	fullText := title + " " + s.Name

	if reMGarbage.MatchString(fullText) {
		return nil, "garbage"
	}
	if e.reExclLang.MatchString(title) {
		return nil, "excl_lang"
	}
	if e.isBlacklisted(title) {
		return nil, "blacklist_title"
	}
	if _, ok := e.blacklist.Hashes[strings.ToLower(s.InfoHash)]; ok {
		return nil, "blacklist_hash"
	}

	seeders := e.extractMovieSeeders(title)
	if seeders < e.weights.MinSeeders {
		return nil, "low_seeders"
	}

	is4K := reM4K.MatchString(fullText)
	is1080p := reM1080p.MatchString(fullText) && !reM720p.MatchString(fullText)

	if !is4K && !is1080p {
		return nil, "resolution_unknown"
	}

	sizeGB := s.SizeGB

	// 4K: accept unknown size with penalty; 1080p: reject unknown
	if is4K {
		if sizeGB != 0 && (sizeGB < float64(e.weights.Min4KGB) || sizeGB > float64(e.weights.Max4KGB)) {
			return nil, "4k_size_oob"
		}
	} else {
		if sizeGB == 0 || sizeGB < float64(e.weights.Min1080pGB) || sizeGB > float64(e.weights.Max1080pGB) {
			return nil, "1080p_size_oob"
		}
	}

	score := e.calculateMovieScore(fullText, seeders, sizeGB, is4K, e.weights)
	if score <= 0 {
		return nil, "zero_score"
	}

	return &MovieStream{
		Title:        title,
		Hash:         strings.ToLower(s.InfoHash),
		Is4K:         is4K,
		QualityScore: score,
		Seeders:      seeders,
		SizeGB:       sizeGB,
		DownloadURL:  s.DownloadURL,
	}, ""
}

func (e *MovieGoEngine) calculateMovieScore(text string, seeders int, sizeGB float64, is4K bool, w config.MovieWeights) int {
	return scoreMovieRelease(text, seeders, sizeGB, is4K, w, e.reITA)
}

// scoreMovieRelease scores a candidate with the movie profile. The watchlist handles movies
// only, so a scale of its own would just be a third set of numbers to keep in sync.
func scoreMovieRelease(text string, seeders int, sizeGB float64, is4K bool, w config.MovieWeights, preferredLang *regexp.Regexp) int {
	score := 0

	if is4K {
		score += w.Res4K
	} else {
		score += w.Res1080p
	}

	if reMDV.MatchString(text) {
		score += w.DolbyVision
	} else if reMHDR.MatchString(text) {
		score += w.HDR
	}

	if reMAtmos.MatchString(text) {
		score += w.Atmos
	} else if reM51.MatchString(text) {
		score += w.Audio51
	} else if reMStereo.MatchString(text) {
		score += w.StereoPenalty
	}

	if reMRemux.MatchString(text) {
		score += w.Remux
	}

	if preferredLang.MatchString(text) {
		score += w.PreferredLanguage
	}

	if sizeGB == 0 && is4K {
		score += w.UnknownSize4KPenalty
	}

	seederBonus := seeders
	if seederBonus > w.SeederCap {
		seederBonus = w.SeederCap
	}
	score += seederBonus

	return score
}

func (e *MovieGoEngine) extractMovieSeeders(title string) int {
	m := reMSeeders.FindStringSubmatch(title)
	if len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func (e *MovieGoEngine) filterVideoFiles(files []FileStat, is4K bool) []FileStat {
	var valid []FileStat
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext != ".mkv" && ext != ".mp4" && ext != ".avi" && ext != ".mov" && ext != ".m4v" {
			continue
		}
		minSize := int64(e.weights.Min4KGB) * 1024 * 1024 * 1024
		maxSize := int64(e.weights.Max4KGB) * 1024 * 1024 * 1024
		if !is4K {
			minSize = int64(e.weights.Min1080pGB) * 1024 * 1024 * 1024
			maxSize = int64(e.weights.Max1080pGB) * 1024 * 1024 * 1024
		}
		if f.Length >= minSize && f.Length <= maxSize {
			valid = append(valid, f)
		}
	}
	return valid
}

func (e *MovieGoEngine) buildMovieFilename(title, releaseDate string, stream MovieStream) string {
	return library.BuildMovieFilename(library.MovieName{
		Title: title, ReleaseDate: releaseDate, ReleaseTitle: stream.Title,
		Is4K: stream.Is4K, Hash: stream.Hash,
	})
}

func (e *MovieGoEngine) sanitizeMovieFilename(s string) string {
	return library.SanitizeMovieName(s)
}

func (e *MovieGoEngine) resolveIMDB(ctx context.Context, tmdbID int, title string) string {
	// Check cache
	if entry, ok := e.imdbCache[strconv.Itoa(tmdbID)]; ok {
		if entry.IMDBID != "" {
			return entry.IMDBID
		}
	}

	imdbID, err := e.tmdb.ExternalIDs(ctx, tmdbID)
	if err != nil || imdbID == "" {
		return ""
	}

	e.imdbCache[strconv.Itoa(tmdbID)] = IMDBCacheEntry{
		IMDBID: imdbID,
		Title:  title,
		TS:     time.Now().Unix(),
	}

	return imdbID
}

func (e *MovieGoEngine) isBlacklisted(title string) bool {
	t := strings.ToLower(title)
	t = reMYear.ReplaceAllString(t, "")
	t = reMQuality.ReplaceAllString(t, "")
	normalized := reMNonWord.ReplaceAllString(t, "")

	for _, bt := range e.blacklist.Titles {
		if bt == normalized {
			return true
		}
	}

	return false
}

func (e *MovieGoEngine) rehydrateMissingTorrents(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}
	activeHashes := make(map[string]bool)
	for _, t := range torrents {
		activeHashes[t.Hash] = true
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var url, magnet, imdbID string
		var size float64
		content := strings.TrimSpace(string(data))

		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ = obj["url"].(string)
			magnet, _ = obj["magnet"].(string)
			size, _ = obj["size"].(float64)
			imdbID, _ = obj["imdb"].(string)
		} else {
			lines := strings.SplitN(content, "\n", 4)
			if len(lines) < 3 {
				return nil
			}
			url = strings.TrimSpace(lines[0])
			magnet = strings.TrimSpace(lines[2])
			if len(lines) > 1 {
				size, _ = strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
			}
			if len(lines) >= 4 {
				imdbID = strings.TrimSpace(lines[3])
			}
		}

		m := reMHashURL.FindStringSubmatch(url)
		if len(m) < 2 {
			return nil
		}
		hash := strings.ToLower(m[1])

		if activeHashes[hash] {
			return nil
		}

		if strings.HasPrefix(magnet, "magnet:?") {
			displayTitle := TitleFromFilename(info.Name())
			freshMagnet := BuildMagnet(hash, displayTitle, DefaultTrackers())
			if _, err := e.gostorm.AddTorrent(ctx, freshMagnet, displayTitle); err == nil {
				// Preserve the original imdb field — previously hardcoded to "", which silently
				// wiped dedup metadata on every rehydration and let buildExistingMovieIndex's
				// imdb=="" skip make the file invisible to future dedup checks (root cause of
				// duplicate movie files after a torrent expired and got rehydrated).
				e.createMKV(path, url, int64(size), freshMagnet, imdbID)
			}
		}

		return nil
	})
}

func (e *MovieGoEngine) cleanupOrphanedFiles(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}
	activeHashes := make(map[string]bool)
	for _, t := range torrents {
		activeHashes[t.Hash] = true
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var url string
		content := strings.TrimSpace(string(data))
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ = obj["url"].(string)
		} else {
			lines := strings.SplitN(content, "\n", 2)
			if len(lines) < 1 {
				return nil
			}
			url = strings.TrimSpace(lines[0])
		}

		m := reMHashURL.FindStringSubmatch(url)
		if len(m) < 2 {
			return nil
		}
		hash := strings.ToLower(m[1])
		if !activeHashes[hash] {
			e.removeStub(ctx, path, hash)
		}
		return nil
	})
}

// Cache helpers
func (e *MovieGoEngine) loadCache(file string) map[string]CacheEntry {
	data, err := os.ReadFile(file)
	if err != nil {
		return make(map[string]CacheEntry)
	}
	var m map[string]CacheEntry
	json.Unmarshal(data, &m)
	return m
}

func (e *MovieGoEngine) loadIMDBCache(file string) map[string]IMDBCacheEntry {
	data, err := os.ReadFile(file)
	if err != nil {
		return make(map[string]IMDBCacheEntry)
	}
	var m map[string]IMDBCacheEntry
	json.Unmarshal(data, &m)
	return m
}

func (e *MovieGoEngine) loadBlacklist() BlacklistData {
	data, err := os.ReadFile(e.blacklistFile)
	if err != nil {
		return BlacklistData{Hashes: make(map[string]string), Titles: []string{}}
	}
	var bl BlacklistData
	json.Unmarshal(data, &bl)
	if bl.Hashes == nil {
		bl.Hashes = make(map[string]string)
	}
	return bl
}

func (e *MovieGoEngine) isInCache(cache map[string]CacheEntry, key string, ttl time.Duration) bool {
	entry, ok := cache[key]
	if !ok {
		return false
	}
	if time.Since(time.Unix(entry.TS, 0)) > ttl {
		delete(cache, key)
		return false
	}
	return true
}

func (e *MovieGoEngine) setCache(cache map[string]CacheEntry, key string, entry CacheEntry) {
	cache[key] = entry
}

func (e *MovieGoEngine) pruneExpiredCaches() {
	now := time.Now()

	for k, v := range e.noMKVCache {
		if now.Sub(time.Unix(v.TS, 0)) > noMKVCacheTTL {
			delete(e.noMKVCache, k)
		}
	}
	for k, v := range e.noStreamsCache {
		if now.Sub(time.Unix(v.TS, 0)) > noStreamsCacheTTL {
			delete(e.noStreamsCache, k)
		}
	}
	for k, v := range e.recheckCache {
		if now.Sub(time.Unix(v.TS, 0)) > recheckCacheTTL {
			delete(e.recheckCache, k)
		}
	}
	for k, v := range e.addFailCache {
		if now.Sub(time.Unix(v.TS, 0)) > addFailCacheTTL {
			delete(e.addFailCache, k)
		}
	}
}

func (e *MovieGoEngine) saveAllCaches() {
	e.saveNoMKVCache()
	e.saveCache(e.noStreamsCFile, e.noStreamsCache)
	e.saveCache(e.recheckCFile, e.recheckCache)
	e.saveCache(e.addFailCFile, e.addFailCache)
	e.saveIMDBCache(e.imdbCFile, e.imdbCache)
}

// isMigratedFile returns true if path+".migrated" exists, indicating the file
// has been ingested into SQLite and the raw JSON should no longer be written.
func isMigratedFile(path string) bool {
	_, err := os.Stat(path + ".migrated")
	return err == nil
}

// loadNoMKVCache reads the hashes to skip from the state DB, falling back to the
// legacy JSON when there is no DB. Without it the cache lives only inside one run,
// which is what happened between the April migration and now: the DB write was
// never implemented and the JSON one had already been turned off.
func (e *MovieGoEngine) loadNoMKVCache() map[string]CacheEntry {
	if e.db == nil {
		return e.loadCache(e.noMKVCFile)
	}
	neg, err := e.db.LoadNegatives()
	if err != nil {
		e.logger.Printf("WARNING: could not read the negative cache from the state DB: %v", err)
		return e.loadCache(e.noMKVCFile)
	}
	out := make(map[string]CacheEntry, len(neg))
	for hash, entry := range neg {
		ts, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil {
			// An unparseable row is one skipped candidate, not a reason to drop the set.
			continue
		}
		out[hash] = CacheEntry{Reason: entry.Reason, TS: ts.Unix()}
	}
	return out
}

// saveNoMKVCache persists the set the run ends with. The JSON file is written only
// when there is no DB: recreating it would make the next startup re-run the JSON
// migration, which clears the table this function just filled.
func (e *MovieGoEngine) saveNoMKVCache() {
	if e.db == nil {
		if !isMigratedFile(e.noMKVCFile) {
			e.saveCache(e.noMKVCFile, e.noMKVCache)
		}
		return
	}
	now := time.Now()
	entries := make([]metadb.NegativeCacheEntry, 0, len(e.noMKVCache))
	for hash, entry := range e.noMKVCache {
		if now.Sub(time.Unix(entry.TS, 0)) > noMKVCacheTTL {
			// Expired in memory but never read again this run: writing it back would
			// resurrect the rows the hourly cleanup has just deleted.
			delete(e.noMKVCache, hash)
			continue
		}
		entries = append(entries, metadb.NegativeCacheEntry{
			Hash:      hash,
			Reason:    entry.Reason,
			Timestamp: time.Unix(entry.TS, 0).UTC().Format(time.RFC3339),
		})
	}
	if err := e.db.ReplaceNegatives(entries); err != nil {
		e.logger.Printf("WARNING: could not save the negative cache to the state DB: %v", err)
	}
}

func (e *MovieGoEngine) saveCache(file string, data interface{}) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	os.WriteFile(tmp, jsonData, 0644)
	os.Rename(tmp, file)
}

func (e *MovieGoEngine) saveIMDBCache(file string, data map[string]IMDBCacheEntry) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	os.WriteFile(tmp, jsonData, 0644)
	os.Rename(tmp, file)
}

func (e *MovieGoEngine) createMKV(path, streamURL string, fileSize int64, magnet, imdbID string) bool {
	return library.WriteStub(path, streamURL, fileSize, magnet, imdbID) == nil
}
