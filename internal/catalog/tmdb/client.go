package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"

	"tiramisu/internal/catalog"
)

const baseURL = "https://api.themoviedb.org/3"

// Client is a TMDB API client with rate limiting.
type Client struct {
	http    *http.Client
	limiter *rate.Limiter
	apiKey  string
}

// NewClient creates a TMDB client with 4 req/s rate limit (40/10s official).
func NewClient(apiKey string) *Client {
	return &Client{
		http:    catalog.NewClient(30 * time.Second),
		limiter: rate.NewLimiter(rate.Every(250*time.Millisecond), 1),
		apiKey:  apiKey,
	}
}

// Movie is a minimal movie entry from TMDB discover/search.
type Movie struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	ReleaseDate   string `json:"release_date"`
	Language      string `json:"original_language"`
}

// DiscoverResponse is the paginated TMDB response.
type DiscoverResponse struct {
	Page    int     `json:"page"`
	Results []Movie `json:"results"`
}

// DiscoverMovies returns movies from TMDB discover endpoint.
func (c *Client) DiscoverMovies(ctx context.Context, lang string, dateGTE, dateLTE string, pages int) ([]Movie, error) {
	var all []Movie
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s/discover/movie?api_key=%s&primary_release_date.gte=%s&primary_release_date.lte=%s&with_original_language=%s&sort_by=popularity.desc&include_adult=false&include_video=false&page=%d",
			baseURL, c.apiKey, dateGTE, dateLTE, lang, page)

		movies, err := c.fetchDiscoverPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, movies...)
	}
	return all, nil
}

// DiscoverMoviesByRegion returns movies from a region-specific endpoint (now_playing, popular).
func (c *Client) DiscoverMoviesByRegion(ctx context.Context, endpoint, region string, pages int) ([]Movie, error) {
	var all []Movie
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s%s?api_key=%s&language=en-US&region=%s&page=%d",
			baseURL, endpoint, c.apiKey, region, page)

		movies, err := c.fetchDiscoverPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, movies...)
	}
	return all, nil
}

// TrendingMovies returns trending movies for the week.
func (c *Client) TrendingMovies(ctx context.Context, pages int) ([]Movie, error) {
	var all []Movie
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s/trending/movie/week?api_key=%s&page=%d",
			baseURL, c.apiKey, page)

		movies, err := c.fetchDiscoverPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, movies...)
	}
	return all, nil
}

// ExternalIDs returns the IMDB ID for a TMDB movie.
func (c *Client) ExternalIDs(ctx context.Context, tmdbID int) (string, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}

	urlStr := fmt.Sprintf("%s/movie/%d/external_ids?api_key=%s", baseURL, tmdbID, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		IMDBID string `json:"imdb_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.IMDBID, nil
}

// TVExternalIDs returns the IMDB ID for a TMDB TV show.
func (c *Client) TVExternalIDs(ctx context.Context, tmdbID int) (string, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}

	urlStr := fmt.Sprintf("%s/tv/%d/external_ids?api_key=%s", baseURL, tmdbID, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		IMDBID string `json:"imdb_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.IMDBID, nil
}

// MovieDetails returns full movie details including watch providers.
func (c *Client) MovieDetails(ctx context.Context, tmdbID int) (*MovieDetail, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	urlStr := fmt.Sprintf("%s/movie/%d?api_key=%s&append_to_response=watch/providers", baseURL, tmdbID, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var detail MovieDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	return &detail, nil
}

// TVDetails returns full TV show details.
func (c *Client) TVDetails(ctx context.Context, tmdbID int) (*TVDetail, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	urlStr := fmt.Sprintf("%s/tv/%d?api_key=%s&append_to_response=watch/providers", baseURL, tmdbID, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var detail TVDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	return &detail, nil
}

// DiscoverTV returns TV shows from TMDB discover.
func (c *Client) DiscoverTV(ctx context.Context, lang string, dateGTE, dateLTE string, pages int) ([]TVShow, error) {
	var all []TVShow
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s/discover/tv?api_key=%s&first_air_date.gte=%s&first_air_date.lte=%s&with_original_language=%s&sort_by=popularity.desc&page=%d",
			baseURL, c.apiKey, dateGTE, dateLTE, lang, page)

		shows, err := c.fetchTVPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, shows...)
	}
	return all, nil
}

// TVOnTheAir returns TV shows currently on the air.
func (c *Client) TVOnTheAir(ctx context.Context, pages int) ([]TVShow, error) {
	return c.fetchTVListEndpoint(ctx, "/tv/on_the_air", pages)
}

// TVAiringToday returns TV shows airing today.
func (c *Client) TVAiringToday(ctx context.Context, pages int) ([]TVShow, error) {
	return c.fetchTVListEndpoint(ctx, "/tv/airing_today", pages)
}

// TVPopular returns popular TV shows.
func (c *Client) TVPopular(ctx context.Context, pages int) ([]TVShow, error) {
	return c.fetchTVListEndpoint(ctx, "/tv/popular", pages)
}

// TVTrending returns trending TV shows for the week.
func (c *Client) TVTrending(ctx context.Context, pages int) ([]TVShow, error) {
	var all []TVShow
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s/trending/tv/week?api_key=%s&page=%d",
			baseURL, c.apiKey, page)

		shows, err := c.fetchTVPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, shows...)
	}
	return all, nil
}

// SearchMovie searches for a movie by query and optional year.
func (c *Client) SearchMovie(ctx context.Context, query, year string) (int, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return 0, err
	}

	urlStr := fmt.Sprintf("%s/search/movie?api_key=%s&query=%s&page=1",
		baseURL, c.apiKey, url.QueryEscape(query))
	if year != "" {
		urlStr += "&year=" + year
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return 0, err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if len(result.Results) == 0 {
		return 0, fmt.Errorf("no results for %q", query)
	}

	return result.Results[0].ID, nil
}

func (c *Client) fetchDiscoverPage(ctx context.Context, urlStr string) ([]Movie, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result DiscoverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Results, nil
}

func (c *Client) fetchTVPage(ctx context.Context, urlStr string) ([]TVShow, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := catalog.Do(ctx, c.http, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Results []TVShow `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Results, nil
}

func (c *Client) fetchTVListEndpoint(ctx context.Context, endpoint string, pages int) ([]TVShow, error) {
	var all []TVShow
	for page := 1; page <= pages; page++ {
		urlStr := fmt.Sprintf("%s%s?api_key=%s&page=%d", baseURL, endpoint, c.apiKey, page)

		shows, err := c.fetchTVPage(ctx, urlStr)
		if err != nil {
			return all, err
		}
		all = append(all, shows...)
	}
	return all, nil
}

// MovieDetail is the full movie details response from TMDB.
type MovieDetail struct {
	ID             int                    `json:"id"`
	Title          string                 `json:"title"`
	ReleaseDate    string                 `json:"release_date"`
	Runtime        int                    `json:"runtime"`
	WatchProviders map[string]interface{} `json:"watch/providers"`
}

// TVShow is a minimal TV show entry from TMDB.
type TVShow struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	FirstAirDate string `json:"first_air_date"`
	Language     string `json:"original_language"`
	GenreIDs     []int  `json:"genre_ids"`
}

// TVDetail is the full TV show details response from TMDB.
type TVDetail struct {
	ID               int         `json:"id"`
	Name             string      `json:"name"`
	FirstAirDate     string      `json:"first_air_date"`
	LastAirDate      string      `json:"last_air_date"`
	NextEpisodeToAir interface{} `json:"next_episode_to_air"`
	NumberOfSeasons  int         `json:"number_of_seasons"`
	Seasons          []Season    `json:"seasons"`
	WatchProviders   interface{} `json:"watch/providers"`
}

// Season represents a TV season.
type Season struct {
	SeasonNumber int `json:"season_number"`
	EpisodeCount int `json:"episode_count"`
}

// HasPremiumProvider checks if a movie/show is available on premium streaming in Italy.
func HasPremiumProvider(providers interface{}) bool {
	if providers == nil {
		return false
	}

	pm, ok := providers.(map[string]interface{})
	if !ok {
		return false
	}

	results, ok := pm["results"].(map[string]interface{})
	if !ok {
		return false
	}

	it, ok := results["IT"].(map[string]interface{})
	if !ok {
		return false
	}

	flatrate, ok := it["flatrate"].([]interface{})
	if !ok {
		return false
	}

	premiumIDs := map[int]bool{8: true, 9: true, 337: true, 531: true}
	for _, p := range flatrate {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if id, ok := pm["provider_id"].(float64); ok && premiumIDs[int(id)] {
			return true
		}
	}

	return false
}
