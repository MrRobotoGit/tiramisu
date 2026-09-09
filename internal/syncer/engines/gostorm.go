package engines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tiramisu/internal/catalog"
	"tiramisu/internal/library"
)

// GoStormClient handles HTTP operations with the GoStorm engine.
type GoStormClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewGoStormClient creates a client for GoStorm API operations.
func NewGoStormClient(baseURL string) *GoStormClient {
	return &GoStormClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// TorrentStats and FileStat live in internal/library: the same shapes are returned by
// the /api/library endpoints, and aliasing them lets *GoStormClient satisfy
// library.GoStorm with no adapter.
type TorrentStats = library.TorrentStats

type FileStat = library.FileStat

// AddTorrent adds a magnet URL to GoStorm via POST /torrents {"action":"add"}.
// Returns the 40-char info hash or empty string on failure.
func (c *GoStormClient) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	m := regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)
	match := m.FindStringSubmatch(magnet)
	if len(match) < 2 {
		return "", fmt.Errorf("cannot extract hash from magnet")
	}
	hash := strings.ToLower(match[1])

	body := map[string]interface{}{
		"action": "add",
		"link":   magnet,
		"title":  title,
		"save":   true,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gostorm error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 120)]))
	}

	// Response contains the torrent object with hash
	var result struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Hash != "" {
		return strings.ToLower(result.Hash), nil
	}

	return hash, nil
}

// GetTorrentInfo polls GoStorm until file_stats appear.
func (c *GoStormClient) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	sleepSeq := []int{1, 2, 3, 3, 3, 5}
	deadline := time.Now().Add(time.Duration(maxWait) * time.Second)
	attempt := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		info, err := c.getTorrent(ctx, hash)
		if err == nil && len(info.FileStats) > 0 {
			return info, nil
		}

		sleep := 3
		if attempt < len(sleepSeq) {
			sleep = sleepSeq[attempt]
		}
		time.Sleep(time.Duration(sleep) * time.Second)
		attempt++
	}

	return nil, fmt.Errorf("metadata timeout for %s… (waited %ds)", hash[:8], maxWait)
}

// RemoveTorrent removes a torrent from GoStorm.
func (c *GoStormClient) RemoveTorrent(ctx context.Context, hash string) error {
	body := map[string]string{"action": "rem", "hash": hash}
	return c.postTorrents(ctx, body)
}

// ListTorrents returns all active torrents.
func (c *GoStormClient) ListTorrents(ctx context.Context) ([]TorrentStats, error) {
	body := map[string]string{"action": "list"}
	data, err := c.doTorrents(ctx, body, true)
	if err != nil {
		return nil, err
	}

	var torrents []TorrentStats
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, err
	}
	return torrents, nil
}

func (c *GoStormClient) getTorrent(ctx context.Context, hash string) (*TorrentStats, error) {
	body := map[string]string{"action": "get", "hash": hash}
	data, err := c.doTorrents(ctx, body, false)
	if err != nil {
		return nil, err
	}

	var info TorrentStats
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// doTorrents posts a JSON action body to GoStorm's /torrents endpoint. retry controls
// whether transient failures are retried (catalog.Do) - true for ListTorrents (a standalone
// read with no other retry mechanism around it), false for getTorrent (already wrapped in
// GetTorrentInfo's own outer polling/backoff loop - stacking catalog.Do's retry inside it
// would waste that loop's time budget for no benefit) and for mutations (add/rem), so a
// mutation that already succeeded server-side is never silently duplicated by a retry.
func (c *GoStormClient) doTorrents(ctx context.Context, body map[string]string, retry bool) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var resp *http.Response
	if retry {
		resp, err = catalog.Do(ctx, c.httpClient, req)
	} else {
		resp, err = c.httpClient.Do(req)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gostorm /torrents: status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (c *GoStormClient) postTorrents(ctx context.Context, body map[string]string) error {
	_, err := c.doTorrents(ctx, body, false)
	return err
}

// TitleFromFilename extracts a clean display title from an MKV filename.
// e.g. "Your_Friends_Neighbors_S01E09_17361ba1.mkv" → "Your Friends Neighbors S01E09"
func TitleFromFilename(filename string) string {
	s := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Remove trailing _hash8 (8 hex chars)
	if re := regexp.MustCompile(`_[a-f0-9]{8}$`); re.MatchString(s) {
		s = s[:len(s)-9]
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	return strings.TrimSpace(s)
}

// BuildMagnet and DefaultTrackers live in internal/library, shared with the
// /api/library endpoints.
func BuildMagnet(infoHash, name string, trackers []string) string {
	return library.BuildMagnet(infoHash, name, trackers)
}

func DefaultTrackers() []string { return library.DefaultTrackers() }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
