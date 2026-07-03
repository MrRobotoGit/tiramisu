package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog"
	"tiramisu/internal/catalog/mediaserver"
	"tiramisu/internal/catalog/tmdb"
	"tiramisu/internal/catalog/torrentio"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/syncer/quality"
)

// WatchlistGoEngine is the pure Go implementation of watchlist sync.
type WatchlistGoEngine struct {
	gostorm    *GoStormClient
	tmdb       *tmdb.Client
	torrentio  *torrentio.Client
	prowlarr   *prowlarr.Client
	mediasrv   mediaserver.Client
	httpClient *http.Client
	moviesDir  string
	plexURL    string
	plexToken  string
	sectionID  int
	limiter    *rate.Limiter
	logger     *log.Logger
}

// WatchlistConfig holds all config needed for the engine.
type WatchlistConfig struct {
	GoStormURL      string
	TMDBAPIKey      string
	TorrentioURL    string
	PlexURL         string
	PlexToken       string
	PlexSection     int
	MoviesDir       string
	MediaServerType string
	LogsDir         string
	ProwlarrCfg     prowlarr.ConfigProwlarr
}

// NewWatchlistGoEngine creates a new Go watchlist sync engine.
func NewWatchlistGoEngine(cfg WatchlistConfig) *WatchlistGoEngine {
	var prowlarrClient *prowlarr.Client
	if cfg.ProwlarrCfg.Enabled {
		prowlarrClient = prowlarr.NewClient(cfg.ProwlarrCfg)
	}

	logPath := filepath.Join(cfg.LogsDir, "watchlist-sync.log")
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "[WatchlistSync] ", log.LstdFlags)

	return &WatchlistGoEngine{
		gostorm:    NewGoStormClient(cfg.GoStormURL),
		tmdb:       tmdb.NewClient(cfg.TMDBAPIKey),
		torrentio:  torrentio.NewClient(cfg.TorrentioURL, "sort=qualitysize|qualityfilter=480p,720p,scr,cam"),
		prowlarr:   prowlarrClient,
		mediasrv:   mediaserver.New(cfg.MediaServerType, cfg.PlexURL, cfg.PlexToken),
		httpClient: catalog.NewClient(20 * time.Second),
		moviesDir:  cfg.MoviesDir,
		plexURL:    cfg.PlexURL,
		plexToken:  cfg.PlexToken,
		sectionID:  cfg.PlexSection,
		limiter:    rate.NewLimiter(rate.Every(500*time.Millisecond), 1),
		logger:     logger,
	}
}

func (e *WatchlistGoEngine) Name() string { return "watchlist" }

func (e *WatchlistGoEngine) Run(ctx context.Context) error {
	e.logger.Printf("[WatchlistSync] Starting sync")
	items, err := e.fetchWatchlist(ctx)
	if err != nil {
		e.logger.Printf("[WatchlistSync] Failed to fetch watchlist: %v", err)
		return fmt.Errorf("fetch watchlist: %w", err)
	}
	e.logger.Printf("[WatchlistSync] Watchlist items: %d", len(items))
	if len(items) == 0 {
		return nil
	}

	imdbSet, titleIndex := e.buildExistingIndex(ctx)

	added := 0
	skipped := 0

	for _, item := range items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if item.Type != "movie" {
			continue
		}

		if item.IMDBID == "" {
			item.IMDBID, err = e.resolveIMDB(ctx, item.Title, item.Year)
			if err != nil || item.IMDBID == "" {
				skipped++
				continue
			}
		}

		if e.isAlreadyPresent(item, imdbSet, titleIndex) {
			continue
		}

		streams, err := e.getStreams(ctx, item.IMDBID, item.Title)
		if err != nil || len(streams) == 0 {
			skipped++
			continue
		}

		candidates := e.pickBestStream(streams)
		if len(candidates) == 0 {
			skipped++
			continue
		}

		mkvCreated := false
		for _, candidate := range candidates {
			infoHash := strings.ToLower(candidate.InfoHash)
			if infoHash == "" {
				continue
			}

			magnet := BuildMagnet(infoHash, item.Title, DefaultTrackers())
			hash, err := e.gostorm.AddTorrent(ctx, magnet, item.Title)
			if err != nil || hash == "" {
				continue
			}

			torrentInfo, err := e.gostorm.GetTorrentInfo(ctx, hash, 25)
			if err != nil {
				e.gostorm.RemoveTorrent(ctx, hash)
				continue
			}

			isBDMV := false
			for _, f := range torrentInfo.FileStats {
				if strings.Contains(strings.ToUpper(f.Path), "BDMV") {
					isBDMV = true
					break
				}
			}
			if isBDMV {
				e.gostorm.RemoveTorrent(ctx, hash)
				continue
			}

			var bestFile *FileStat
			for i := range torrentInfo.FileStats {
				f := &torrentInfo.FileStats[i]
				if strings.HasSuffix(strings.ToLower(f.Path), ".mkv") {
					if bestFile == nil || f.Length > bestFile.Length {
						bestFile = f
					}
				}
			}
			if bestFile == nil {
				e.gostorm.RemoveTorrent(ctx, hash)
				continue
			}

			mkvPath, err := e.createMKV(hash, candidate.Title, bestFile.ID, bestFile.Length, magnet, item.IMDBID, item.Title, item.Year)
			if err != nil || mkvPath == "" {
				continue
			}

			added++
			mkvCreated = true
			break
		}

		if !mkvCreated {
			skipped++
		}

		if err := e.limiter.Wait(ctx); err != nil {
			return ctx.Err()
		}
	}

	e.logger.Printf("[WatchlistSync] Done: %d added, %d skipped", added, skipped)
	if added > 0 {
		e.mediasrv.RefreshLibrary(context.Background(), e.sectionID)
	}

	return nil
}

type WatchlistItem struct {
	Title  string
	Year   string
	IMDBID string
	Type   string
}

func (e *WatchlistGoEngine) fetchWatchlist(ctx context.Context) ([]WatchlistItem, error) {
	url := fmt.Sprintf("https://discover.provider.plex.tv/library/sections/watchlist/all?X-Plex-Token=%s&X-Plex-Platform=Web&format=json", e.plexToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := catalog.Do(ctx, e.httpClient, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		MediaContainer struct {
			Metadata []map[string]interface{} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	var items []WatchlistItem
	for _, m := range result.MediaContainer.Metadata {
		title, _ := m["title"].(string)
		year := ""
		if y, ok := m["year"].(float64); ok {
			year = fmt.Sprintf("%.0f", y)
		}
		typ, _ := m["type"].(string)

		imdbID := ""
		if guids, ok := m["Guid"].([]interface{}); ok {
			for _, g := range guids {
				if gm, ok := g.(map[string]interface{}); ok {
					if id, ok := gm["id"].(string); ok && strings.HasPrefix(id, "imdb://") {
						imdbID = strings.TrimPrefix(id, "imdb://")
						break
					}
				}
			}
		}

		items = append(items, WatchlistItem{
			Title:  strings.TrimSpace(title),
			Year:   year,
			IMDBID: imdbID,
			Type:   strings.ToLower(typ),
		})
	}

	return items, nil
}

func (e *WatchlistGoEngine) resolveIMDB(ctx context.Context, title, year string) (string, error) {
	if err := e.limiter.Wait(ctx); err != nil {
		return "", err
	}

	tmdbID, err := e.tmdb.SearchMovie(ctx, title, year)
	if err != nil {
		return "", err
	}

	return e.tmdb.ExternalIDs(ctx, tmdbID)
}

func (e *WatchlistGoEngine) buildExistingIndex(ctx context.Context) (map[string]bool, map[string]string) {
	imdbSet := make(map[string]bool)
	titleIndex := make(map[string]string)

	reYear := regexp.MustCompile(`[._]((?:19|20)\d{2})[._]`)
	reIMDB := regexp.MustCompile(`^tt\d+$`)

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil || len(data) > 10240 {
			stem := strings.TrimSuffix(filepath.Base(path), ".mkv")
			if m := reYear.FindStringSubmatch(stem); len(m) > 1 {
				yr := m[1]
				idx := strings.Index(stem, m[1])
				if idx > 0 {
					base := regexp.MustCompile(`\W+`).ReplaceAllString(stem[:idx], "")
					titleIndex[base+yr] = path
				}
			}
			return nil
		}

		content := strings.TrimSpace(string(data))
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(content), &obj); err == nil {
				if size, ok := obj["size"].(float64); ok && size == 0 {
					return nil
				}
				if imdb, ok := obj["imdb"].(string); ok && reIMDB.MatchString(imdb) {
					imdbSet[imdb] = true
				}
			}
		} else {
			lines := strings.Split(content, "\n")
			if len(lines) >= 4 {
				sizeVal := strings.TrimSpace(lines[1])
				imdbVal := strings.TrimSpace(lines[3])
				if sizeVal == "0" || sizeVal == "" {
					return nil
				}
				if reIMDB.MatchString(imdbVal) {
					imdbSet[imdbVal] = true
				}
			}
		}

		stem := strings.TrimSuffix(filepath.Base(path), ".mkv")
		if m := reYear.FindStringSubmatch(stem); len(m) > 1 {
			yr := m[1]
			idx := strings.Index(stem, m[1])
			if idx > 0 {
				base := regexp.MustCompile(`\W+`).ReplaceAllString(stem[:idx], "")
				titleIndex[base+yr] = path
			}
		}

		return nil
	})

	return imdbSet, titleIndex
}

func (e *WatchlistGoEngine) isAlreadyPresent(item WatchlistItem, imdbSet map[string]bool, titleIndex map[string]string) bool {
	if item.IMDBID != "" && imdbSet[item.IMDBID] {
		return true
	}
	norm := regexp.MustCompile(`\W+`).ReplaceAllString(strings.ToLower(item.Title), "") + item.Year
	return titleIndex[norm] != ""
}

func (e *WatchlistGoEngine) getStreams(ctx context.Context, imdbID, title string) ([]prowlarr.Stream, error) {
	if e.prowlarr != nil {
		streams := e.prowlarr.FetchTorrents(imdbID, "movie", title)
		if len(streams) > 0 {
			return streams, nil
		}
	}

	tioStreams, err := e.torrentio.FetchMovieStreams(ctx, imdbID)
	if err != nil {
		return nil, err
	}

	var streams []prowlarr.Stream
	for _, s := range tioStreams {
		streams = append(streams, prowlarr.Stream{
			Name:     s.Name,
			Title:    s.Title,
			InfoHash: s.InfoHash,
		})
	}

	return streams, nil
}

const (
	movie4KMinGB    = 9.0
	movie4KMaxGB    = 50.0
	movie1080PMinGB = 4.0
	movie1080PMaxGB = 20.0
	minSeeders      = 10
)

var (
	reExcludedLangs = regexp.MustCompile(`🇪🇸|🇫🇷|🇩🇪|🇷🇺|🇨🇳|🇯🇵|🇰🇷|🇹🇭|🇵🇹|🇧🇷|🇺🇦|🇵🇱|🇳🇱|🇹🇷|🇸🇦|🇮🇳|🇨🇿|🇭🇺|🇷🇴`)
	reExcludedDubs  = regexp.MustCompile(`(?i)\b(Ukr|Ukrainian|Ger|German|Fra|French|Spa|Spanish|Por|Portuguese|Rus|Russian|Chi|Chinese|Pol|Polish|Tur|Turkish|Ara|Arabic|Hin|Hindi|Cze|Czech|Hun|Hungarian)\s+Dub\b`)
	reGarbage       = regexp.MustCompile(`(?i)webscreener|screener|\bscr\b|\bcam\b|camrip|hdcam|telesync|\bts\b|telecine|\btc\b`)
	re4K            = regexp.MustCompile(`(?i)2160p|4[kK]|uhd`)
	re1080p         = regexp.MustCompile(`1080p`)
	reSize          = regexp.MustCompile(`(?i)💾\s*([\d.]+)\s*(GB|MB)`)
	reSeeders       = regexp.MustCompile(`👤\s*(\d+)`)
)

func extractGB(title string) float64 {
	m := reSize.FindStringSubmatch(title)
	if len(m) >= 3 {
		v := parseFloat(m[1])
		if strings.EqualFold(m[2], "GB") {
			return v
		}
		return v / 1000.0
	}
	return 0.0
}

func extractSeeders(title string) int {
	m := reSeeders.FindStringSubmatch(title)
	if len(m) > 1 {
		return parseInt(m[1])
	}
	return 0
}

func (e *WatchlistGoEngine) pickBestStream(streams []prowlarr.Stream) []prowlarr.Stream {
	type scored struct {
		stream prowlarr.Stream
		score  int
		is4K   bool
	}

	var candidates []scored
	for _, s := range streams {
		title := s.Title
		if extractSeeders(title) < minSeeders {
			continue
		}
		if reExcludedLangs.MatchString(title) {
			continue
		}
		if reExcludedDubs.MatchString(title) {
			continue
		}
		if reGarbage.MatchString(title) {
			continue
		}

		gb := extractGB(title)
		is4K := re4K.MatchString(title)

		if is4K {
			if gb != 0 && (gb < movie4KMinGB || gb > movie4KMaxGB) {
				continue
			}
		} else {
			if gb == 0 || gb < movie1080PMinGB || gb > movie1080PMaxGB {
				continue
			}
		}

		sc := quality.Score(title, extractSeeders(title), quality.DefaultMovieProfile())
		if sc <= 0 {
			continue
		}

		candidates = append(candidates, scored{stream: s, score: sc, is4K: is4K})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[j].score < candidates[i].score
	})

	var result []prowlarr.Stream
	for _, c := range candidates {
		if c.is4K {
			result = append(result, c.stream)
		}
	}
	for _, c := range candidates {
		if !c.is4K {
			result = append(result, c.stream)
		}
	}

	return result
}

func (e *WatchlistGoEngine) createMKV(hash, streamTitle string, fileIndex int, fileSize int64, magnet, imdbID, movieTitle, year string) (string, error) {
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", e.gostorm.baseURL, hash, fileIndex)

	qtag := ""
	st := strings.ToLower(streamTitle)
	if re4K.MatchString(st) {
		qtag = "2160p"
	} else if re1080p.MatchString(st) {
		qtag = "1080p"
	}
	if regexp.MustCompile(`(?i)\bdv\b|dovi|dolby.?vision`).MatchString(st) {
		qtag += "_DV"
	} else if regexp.MustCompile(`(?i)hdr|hdr10\+?`).MatchString(st) {
		qtag += "_HDR"
	}

	base := cleanFilename(movieTitle + "_" + year)
	if qtag != "" {
		base += "_" + qtag
	}
	filename := fmt.Sprintf("%s_%s.mkv", base, hash[len(hash)-8:])
	path := filepath.Join(e.moviesDir, filename)

	data := map[string]interface{}{
		"url":    streamURL,
		"size":   fileSize,
		"magnet": magnet,
		"imdb":   imdbID,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(e.moviesDir, 0755); err != nil {
		return "", err
	}

	if err := os.WriteFile(path, jsonData, 0644); err != nil {
		return "", err
	}

	return path, nil
}

func cleanFilename(s string) string {
	s = regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(s, "_")
	s = regexp.MustCompile(`_+`).ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func parseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}
