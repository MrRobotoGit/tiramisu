package torrentio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog"
	"tiramisu/internal/prowlarr"
)

// ErrCloudflareChallenge marks an egress-level block. The fallback URL sits behind the same
// Cloudflare edge, so retrying it would only spend another rate-limit slot before failing.
var ErrCloudflareChallenge = errors.New("torrentio cloudflare challenge")

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
		if errors.Is(err, ErrCloudflareChallenge) {
			// Same edge, same block: the fallback URL would be challenged too.
			return nil, err
		}
		// Fallback: retry without config
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

	if resp.StatusCode == 403 || strings.Contains(strings.ToLower(string(data)), "cloudflare") {
		// The curl fallback that used to live here could never work: curl is absent from the
		// published Docker image, and from datacenter/VPN egress curl receives the same
		// Cloudflare challenge as the Go client. Report the block explicitly instead. The body
		// check is case-insensitive because challenge pages spell "Cloudflare" capitalised and
		// part of them is served with status 200.
		return nil, fmt.Errorf("%w (HTTP %d), egress IP likely blocked; use Prowlarr from a non-datacenter egress", ErrCloudflareChallenge, resp.StatusCode)
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

// ToProwlarrStream converts a parsed Torrentio Stream to prowlarr.Stream format.
func (s Stream) ToProwlarrStream() prowlarr.Stream {
	return prowlarr.Stream{
		Name:     s.Name,
		Title:    s.Title,
		InfoHash: s.InfoHash,
	}
}
