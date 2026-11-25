package prowlarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// capsTTL is how long the indexers' search capabilities are trusted: they change only
// when an indexer is added, removed or reconfigured in Prowlarr. capsTimeout bounds the
// fetch, which runs inside a search's own deadline: a slow answer must not consume it.
// A failed fetch is remembered for capsRetry, so a hung Prowlarr costs one capsTimeout
// per minute instead of one per search.
const (
	capsTTL     = time.Hour
	capsTimeout = 5 * time.Second
	capsRetry   = time.Minute
)

// indexerCaps splits the enabled torrent indexers by whether they search by IMDb id,
// for movies and for series. Both lists empty means nothing is known.
type indexerCaps struct {
	movieIMDb, movieText []int
	tvIMDb, tvText       []int
}

type indexerResource struct {
	ID           int    `json:"id"`
	Enable       bool   `json:"enable"`
	Protocol     string `json:"protocol"`
	Capabilities struct {
		MovieSearchParams []string `json:"movieSearchParams"`
		TvSearchParams    []string `json:"tvSearchParams"`
	} `json:"capabilities"`
}

func hasIMDb(params []string) bool {
	for _, p := range params {
		if strings.EqualFold(p, "imdbId") {
			return true
		}
	}
	return false
}

// capabilities returns the cached split, refreshing it past capsTTL. On failure it
// returns the zero value, and the search goes to every indexer as free text.
func (c *Client) capabilities(ctx context.Context) indexerCaps {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	if !c.capsAt.IsZero() && time.Since(c.capsAt) < capsTTL {
		return c.caps
	}
	if !c.capsFailAt.IsZero() && time.Since(c.capsFailAt) < capsRetry {
		return indexerCaps{}
	}
	caps, err := c.fetchCapabilities(ctx)
	if err != nil {
		c.capsFailAt = time.Now()
		return indexerCaps{}
	}
	c.caps, c.capsAt = caps, time.Now()
	// Indexers are probed again with the IMDb id as free text after every refresh.
	c.idTextMu.Lock()
	c.idText = nil
	c.idTextMu.Unlock()
	return caps
}

// idTextMissLimit is how many searches in which an indexer finds the title by name but
// nothing by its IMDb id it takes to stop sending it the id as free text. Most indexers
// never match an id written in the query (on ours only The Pirate Bay does), and each
// useless query is one more request through the indexer, FlareSolverr included.
const idTextMissLimit = 3

type idTextStat struct{ hits, misses int }

// idTextTargets returns the indexers still worth the IMDb id as free text.
func (c *Client) idTextTargets(ids []int) []int {
	c.idTextMu.Lock()
	defer c.idTextMu.Unlock()
	var out []int
	for _, id := range ids {
		if st := c.idText[id]; st != nil && st.hits == 0 && st.misses >= idTextMissLimit {
			continue
		}
		out = append(out, id)
	}
	return out
}

// recordIDText scores each indexer's free-text id request: a hit when it returned
// releases, a miss when it answered with none while its title queries in the same search
// found some. A failed request, or an indexer that found nothing either way, says
// nothing about the id search.
func (c *Client) recordIDText(imdbID string, requests []map[string]string, group []int, results [][]ProwlarrResult, failed []bool) {
	titleFound := map[string]bool{}
	for i, req := range requests {
		if group[i] > 0 && len(results[i]) > 0 {
			titleFound[req["indexerIds"]] = true
		}
	}
	c.idTextMu.Lock()
	defer c.idTextMu.Unlock()
	for i, req := range requests {
		if group[i] != 0 || req["query"] != imdbID || failed[i] {
			continue
		}
		id, err := strconv.Atoi(req["indexerIds"])
		if err != nil {
			continue
		}
		hit := len(results[i]) > 0
		if !hit && !titleFound[req["indexerIds"]] {
			continue
		}
		if c.idText == nil {
			c.idText = map[int]*idTextStat{}
		}
		st := c.idText[id]
		if st == nil {
			st = &idTextStat{}
			c.idText[id] = st
		}
		if hit {
			st.hits++
		} else {
			st.misses++
		}
	}
}

func (c *Client) fetchCapabilities(ctx context.Context) (indexerCaps, error) {
	ctx, cancel := context.WithTimeout(ctx, capsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/api/v1/indexer", nil)
	if err != nil {
		return indexerCaps{}, err
	}
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return indexerCaps{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return indexerCaps{}, fmt.Errorf("indexer list: status %d", resp.StatusCode)
	}
	var list []indexerResource
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return indexerCaps{}, err
	}
	var caps indexerCaps
	for _, ix := range list {
		if !ix.Enable || !strings.EqualFold(ix.Protocol, "torrent") {
			continue
		}
		if hasIMDb(ix.Capabilities.MovieSearchParams) {
			caps.movieIMDb = append(caps.movieIMDb, ix.ID)
		} else {
			caps.movieText = append(caps.movieText, ix.ID)
		}
		if hasIMDb(ix.Capabilities.TvSearchParams) {
			caps.tvIMDb = append(caps.tvIMDb, ix.ID)
		} else {
			caps.tvText = append(caps.tvText, ix.ID)
		}
	}
	return caps, nil
}

func indexerList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}
