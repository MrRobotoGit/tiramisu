// Package rottentomatoes scrapes the public "Movies at Home" list (recent digital/streaming
// releases). Rotten Tomatoes has no public API and the visible poster grid is client-side
// rendered, but the server-rendered HTML embeds a schema.org ItemList JSON-LD block with the
// same titles for SEO purposes, so a plain HTTP GET is enough — no headless browser needed.
package rottentomatoes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tiramisu/internal/catalog"
)

const moviesAtHomeURL = "https://www.rottentomatoes.com/browse/movies_at_home/"

// Entry is a single title from the "Movies at Home" list.
type Entry struct {
	Title string
	Year  string
}

type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: catalog.NewClient(15 * time.Second)}
}

// FetchMoviesAtHome returns the titles currently listed on the "Movies at Home" page.
func (c *Client) FetchMoviesAtHome(ctx context.Context) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, moviesAtHomeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	data, err := catalog.ReadAll(resp)
	if err != nil {
		return nil, err
	}

	return parseItemList(string(data))
}

func parseItemList(html string) ([]Entry, error) {
	idx := strings.Index(html, `"@type":"ItemList"`)
	if idx == -1 {
		return nil, fmt.Errorf("rottentomatoes: ItemList JSON-LD block not found")
	}
	scriptStart := strings.LastIndex(html[:idx], "<script")
	if scriptStart == -1 {
		return nil, fmt.Errorf("rottentomatoes: enclosing <script> tag not found")
	}
	contentStart := strings.Index(html[scriptStart:], ">")
	if contentStart == -1 {
		return nil, fmt.Errorf("rottentomatoes: malformed script tag")
	}
	contentStart += scriptStart + 1
	scriptEnd := strings.Index(html[contentStart:], "</script>")
	if scriptEnd == -1 {
		return nil, fmt.Errorf("rottentomatoes: script closing tag not found")
	}
	jsonStr := strings.TrimSpace(html[contentStart : contentStart+scriptEnd])

	var parsed struct {
		ItemListElement struct {
			ItemListElement []struct {
				Name        string `json:"name"`
				DateCreated string `json:"dateCreated"`
			} `json:"itemListElement"`
		} `json:"itemListElement"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, fmt.Errorf("rottentomatoes: parse ItemList JSON-LD: %w", err)
	}

	entries := make([]Entry, 0, len(parsed.ItemListElement.ItemListElement))
	for _, it := range parsed.ItemListElement.ItemListElement {
		if it.Name == "" {
			continue
		}
		entries = append(entries, Entry{Title: it.Name, Year: it.DateCreated})
	}
	return entries, nil
}
