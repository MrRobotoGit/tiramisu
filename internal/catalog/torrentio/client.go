package torrentio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog"
	"tiramisu/internal/prowlarr"
)

// Client is a Torrentio API client with rate limiting.
type Client struct {
	http     *http.Client
	limiter  *rate.Limiter
	baseURL  string
	config   string
	fallback string
}

// NewClient creates a Torrentio client with 1 req/s rate limit.
func NewClient(baseURL, config string) *Client {
	return &Client{
		http:     catalog.NewClient(30 * time.Second),
		limiter:  rate.NewLimiter(rate.Every(1*time.Second), 1),
		baseURL:  strings.TrimRight(baseURL, "/"),
		config:   config,
		fallback: strings.TrimRight(baseURL, "/"),
	}
}

// Stream represents a parsed Torrentio stream entry.
type Stream struct {
	InfoHash   string
	Title      string
	Seeders    int
	Leechers   int
	Size       int64
	Resolution string
	Name       string
}

// FetchMovieStreams fetches streams for a movie by IMDB ID.
func (c *Client) FetchMovieStreams(ctx context.Context, imdbID string) ([]Stream, error) {
	return c.fetchStreams(ctx, "movie", imdbID, "", 0, 0)
}

// FetchEpisodeStreams fetches streams for a TV episode.
func (c *Client) FetchEpisodeStreams(ctx context.Context, imdbID string, season, episode int) ([]Stream, error) {
	return c.fetchStreams(ctx, "series", imdbID, "", season, episode)
}

func (c *Client) fetchStreams(ctx context.Context, contentType, imdbID string, _ string, season, episode int) ([]Stream, error) {
	var url string
	if contentType == "movie" {
		url = fmt.Sprintf("%s/%s/stream/movie/%s.json", c.baseURL, c.config, imdbID)
	} else {
		url = fmt.Sprintf("%s/%s/stream/series/%s:%d:%d.json", c.baseURL, c.config, imdbID, season, episode)
	}

	streams, err := c.doFetch(ctx, url)
	if err != nil {
		// Fallback: retry without config (Cloudflare bypass)
		fbURL := fmt.Sprintf("%s/stream/%s/%s.json", c.fallback, contentType, imdbID)
		if contentType == "series" {
			fbURL = fmt.Sprintf("%s/stream/series/%s:%d:%d.json", c.fallback, imdbID, season, episode)
		}
		streams, err = c.doFetch(ctx, fbURL)
		if err != nil {
			return nil, err
		}
	}

	return streams, nil
}

func (c *Client) doFetch(ctx context.Context, url string) ([]Stream, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}

	data, err := catalog.ReadAll(resp)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 403 || strings.Contains(string(data), "cloudflare") {
		return c.doFetchViaCurl(ctx, url)
	}

	var result struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var streams []Stream
	for _, raw := range result.Streams {
		s, err := parseStream(raw)
		if err != nil {
			continue
		}
		streams = append(streams, s)
	}

	return streams, nil
}

// doFetchViaCurl is a fallback for when Go's HTTP client is blocked by Cloudflare.
// curl uses a browser-like TLS fingerprint that bypasses the challenge.
func (c *Client) doFetchViaCurl(ctx context.Context, url string) ([]Stream, error) {
	out, err := exec.CommandContext(ctx, "curl", "-s", "-L",
		"-H", "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		url).Output()
	if err != nil {
		return nil, fmt.Errorf("curl fallback: %w", err)
	}
	var result struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("curl fallback parse: %w", err)
	}
	var streams []Stream
	for _, raw := range result.Streams {
		s, err := parseStream(raw)
		if err != nil {
			continue
		}
		streams = append(streams, s)
	}
	return streams, nil
}

var (
	reSeeders  = regexp.MustCompile(`👤\s*(\d+)`)
	reLeechers = regexp.MustCompile(`⬇️\s*(\d+)`)
	reSize     = regexp.MustCompile(`💾\s*([\d.]+)\s*(GB|MB)`)
	reRes      = regexp.MustCompile(`(?i)(2160p|1080p|720p|480p|4k|uhd)`)
)

func parseStream(raw json.RawMessage) (Stream, error) {
	var s struct {
		Name     string `json:"name"`
		Title    string `json:"title"`
		InfoHash string `json:"infoHash"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Stream{}, err
	}

	stream := Stream{
		InfoHash: s.InfoHash,
		Name:     s.Name,
		Title:    s.Title,
	}

	// Parse seeders
	if m := reSeeders.FindStringSubmatch(s.Title); len(m) > 1 {
		stream.Seeders, _ = strconv.Atoi(m[1])
	}

	// Parse leechers
	if m := reLeechers.FindStringSubmatch(s.Title); len(m) > 1 {
		stream.Leechers, _ = strconv.Atoi(m[1])
	}

	// Parse size
	if m := reSize.FindStringSubmatch(s.Title); len(m) > 2 {
		size, _ := strconv.ParseFloat(m[1], 64)
		if strings.EqualFold(m[2], "GB") {
			stream.Size = int64(size * 1024 * 1024 * 1024)
		} else {
			stream.Size = int64(size * 1024 * 1024)
		}
	}

	// Parse resolution
	if m := reRes.FindStringSubmatch(s.Title); len(m) > 1 {
		stream.Resolution = strings.ToLower(m[1])
		if stream.Resolution == "4k" || stream.Resolution == "uhd" {
			stream.Resolution = "2160p"
		}
	}

	return stream, nil
}

// FetchWithProwlarr fetches streams from Prowlarr first, falling back to Torrentio.
func FetchWithProwlarr(ctx context.Context, prowlarrClient *prowlarr.Client, torrentioClient *Client, imdbID, contentType, title string) ([]prowlarr.Stream, error) {
	// Primary: Prowlarr
	if prowlarrClient != nil {
		streams := prowlarrClient.FetchTorrents(imdbID, contentType, title)
		if len(streams) > 0 {
			return streams, nil
		}
	}

	// Fallback: Torrentio
	var streams []prowlarr.Stream
	var torrentioStreams []Stream
	var err error

	if contentType == "movie" {
		torrentioStreams, err = torrentioClient.FetchMovieStreams(ctx, imdbID)
	} else {
		// For TV, we need season/episode — return empty since caller must provide them
		return nil, fmt.Errorf("TV fallback requires season/episode info")
	}

	if err != nil {
		return nil, err
	}

	for _, s := range torrentioStreams {
		streams = append(streams, prowlarr.Stream{
			Name:     s.Name,
			Title:    s.Title,
			InfoHash: s.InfoHash,
		})
	}

	return streams, nil
}

// ToProwlarrStream converts a parsed Torrentio Stream to prowlarr.Stream format.
func (s Stream) ToProwlarrStream() prowlarr.Stream {
	return prowlarr.Stream{
		Name:     s.Name,
		Title:    s.Title,
		InfoHash: s.InfoHash,
	}
}
