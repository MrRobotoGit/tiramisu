// Package enginestest fakes the engine side of engines.GoStormClient for tests: HTTP speaks
// the JSON of the retired :8090 API to an httptest server. Never imported by the binary.
package enginestest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"tiramisu/internal/library"
)

// HTTP returns a Backend (engines.Backend) that posts to baseURL.
func HTTP(baseURL string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

type Client struct {
	base string
	hc   *http.Client
}

func (c *Client) Add(ctx context.Context, link, title string) (string, bool, error) {
	data, _ := json.Marshal(map[string]any{"action": "add", "link": link, "title": title, "save_to_db": true})
	body, err := c.post(ctx, "/torrents", "application/json", bytes.NewReader(data), true)
	if err != nil {
		return "", false, err
	}
	var r struct {
		Hash string `json:"hash"`
	}
	if json.Unmarshal(body, &r) == nil && r.Hash != "" {
		return r.Hash, true, nil
	}
	return "", false, nil
}

func (c *Client) AddTorrentFile(ctx context.Context, data []byte, title string) (string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("save", "true")
	if title != "" {
		_ = w.WriteField("title", title)
	}
	part, _ := w.CreateFormFile("file", "release.torrent")
	_, _ = part.Write(data)
	_ = w.Close()
	body, err := c.post(ctx, "/torrent/upload", w.FormDataContentType(), &b, true)
	if err != nil {
		return "", err
	}
	var r struct {
		Hash string `json:"hash"`
	}
	if json.Unmarshal(body, &r) != nil || r.Hash == "" {
		return "", fmt.Errorf("upload: no hash in the answer")
	}
	return r.Hash, nil
}

// Info polls "get" until file_stats appear or ctx ends, as the old client did.
func (c *Client) Info(ctx context.Context, hash string) (*library.TorrentStats, error) {
	sleeps := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second, 5 * time.Second}
	for i := 0; ; i++ {
		var info library.TorrentStats
		body, err := c.action(ctx, map[string]string{"action": "get", "hash": hash})
		if err == nil && json.Unmarshal(body, &info) == nil && len(info.FileStats) > 0 {
			return &info, nil
		}
		d := 3 * time.Second
		if i < len(sleeps) {
			d = sleeps[i]
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
	}
}

func (c *Client) Remove(ctx context.Context, hash string) error {
	_, err := c.action(ctx, map[string]string{"action": "rem", "hash": hash})
	return err
}

func (c *Client) Refs(ctx context.Context) ([]library.TorrentRef, error) {
	body, err := c.action(ctx, map[string]string{"action": "hashes"})
	if err != nil {
		return nil, err
	}
	var refs []library.TorrentRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (c *Client) Hydrated(ctx context.Context) ([]string, error) {
	body, err := c.action(ctx, map[string]string{"action": "active"})
	if err != nil {
		return nil, err
	}
	var refs []library.TorrentRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, err
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Hash
	}
	return out, nil
}

func (c *Client) action(ctx context.Context, body map[string]string) ([]byte, error) {
	data, _ := json.Marshal(body)
	return c.post(ctx, "/torrents", "application/json", bytes.NewReader(data), false)
}

// post returns the body of a 2xx answer (any 2xx when lenient, else 200 only).
func (c *Client) post(ctx context.Context, path, ctype string, body io.Reader, lenient bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 || (!lenient && resp.StatusCode != http.StatusOK) {
		return nil, fmt.Errorf("engine %s: status %d: %s", path, resp.StatusCode, b[:min(len(b), 120)])
	}
	return b, nil
}
