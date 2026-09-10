package mediaserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"tiramisu/internal/catalog"
)

// Client refreshes a media server library section. Each implementation decides what it
// needs: Plex addresses one section and does nothing without an ID, Jellyfin refreshes
// everything and ignores the argument, so callers pass whatever they have and let the
// client skip the call when it cannot make it.
type Client interface {
	RefreshLibrary(ctx context.Context, sectionID int) error
}

// New creates the appropriate client based on server type.
func New(serverType, url, token string) Client {
	switch serverType {
	case "jellyfin":
		return &JellyfinClient{
			http:  catalog.NewClient(15 * time.Second),
			URL:   url,
			Token: token,
		}
	default:
		return &PlexClient{
			http:  catalog.NewClient(15 * time.Second),
			URL:   url,
			Token: token,
		}
	}
}

// PlexClient implements Client for Plex Media Server.
type PlexClient struct {
	http  *http.Client
	URL   string
	Token string
}

// RefreshLibrary triggers a Plex library scan. A section ID of 0 means "not configured",
// the documented way to leave the refresh off.
func (c *PlexClient) RefreshLibrary(ctx context.Context, sectionID int) error {
	if c.URL == "" || c.Token == "" || sectionID <= 0 {
		return nil
	}

	urlStr := fmt.Sprintf("%s/library/sections/%d/refresh?X-Plex-Token=%s", c.URL, sectionID, c.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plex refresh: status %d", resp.StatusCode)
	}

	return nil
}

// JellyfinClient implements Client for Jellyfin Media Server.
type JellyfinClient struct {
	http  *http.Client
	URL   string
	Token string
}

// RefreshLibrary triggers a Jellyfin library scan. sectionID is ignored: the endpoint
// refreshes every library, which is why a Jellyfin setup leaves the section IDs at 0.
func (c *JellyfinClient) RefreshLibrary(ctx context.Context, _ int) error {
	if c.URL == "" || c.Token == "" {
		return nil
	}

	urlStr := fmt.Sprintf("%s/Library/Refresh", c.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, nil)
	if err != nil {
		return err
	}
	// Jellyfin 12.0 stopped parsing X-Emby-Token; the MediaBrowser scheme works on 10.x too.
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", c.Token))

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin refresh: status %d", resp.StatusCode)
	}

	return nil
}
