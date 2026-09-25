package musicimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeezerArtist is one artist as Deezer describes it; Fans measures how known it is.
type DeezerArtist struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Fans int    `json:"nb_fan"`
}

// Deezer reads the public artist endpoints, which need no key. Its related artists
// come from Deezer's own listening, dense even for small artists, and in a stable
// order, unlike the ListenBrainz radio.
type Deezer struct {
	baseURL string
	rate    time.Duration
	http    *http.Client

	retryDelay time.Duration

	mu   sync.Mutex
	next time.Time
}

func NewDeezer() *Deezer {
	return &Deezer{
		baseURL: "https://api.deezer.com",
		rate:    150 * time.Millisecond, // the quota is 50 requests every 5 seconds
		http:    &http.Client{Timeout: 30 * time.Second},

		retryDelay: 5 * time.Second,
	}
}

// FindArtist resolves a name to the Deezer artist of that exact name with the most
// fans: namesakes are nearly always smaller.
func (d *Deezer) FindArtist(ctx context.Context, name string) (DeezerArtist, bool, error) {
	var result struct {
		Data []DeezerArtist `json:"data"`
	}
	if err := d.get(ctx, "/search/artist", url.Values{"q": {name}, "limit": {"10"}}, &result); err != nil {
		return DeezerArtist{}, false, err
	}
	var best DeezerArtist
	key := artistIdentity(name)
	for _, a := range result.Data {
		if artistIdentity(a.Name) == key && a.Fans >= best.Fans {
			best = a
		}
	}
	return best, best.ID != 0, nil
}

// RelatedArtists lists the artists Deezer relates to one artist, closest first.
func (d *Deezer) RelatedArtists(ctx context.Context, id int) ([]DeezerArtist, error) {
	var result struct {
		Data []DeezerArtist `json:"data"`
	}
	if err := d.get(ctx, "/artist/"+strconv.Itoa(id)+"/related", url.Values{"limit": {"25"}}, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

var errDeezerQuota = errors.New("deezer: quota exceeded")

// get retries a quota answer once the window has passed. Deezer answers errors with
// status 200 and an error object, so the body decides.
func (d *Deezer) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(d.retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err = d.getOnce(ctx, path, query, out); !errors.Is(err, errDeezerQuota) {
			return err
		}
	}
	return err
}

func (d *Deezer) getOnce(ctx context.Context, path string, query url.Values, out interface{}) error {
	if err := d.wait(ctx); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := d.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer %s: status %d", path, response.StatusCode)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		return fmt.Errorf("deezer %s: decode: %w", path, err)
	}
	var failure struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &failure) == nil && failure.Error != nil {
		if failure.Error.Code == 4 || strings.Contains(strings.ToLower(failure.Error.Message), "quota") {
			return fmt.Errorf("deezer %s: %s: %w", path, failure.Error.Message, errDeezerQuota)
		}
		return fmt.Errorf("deezer %s: %s", path, failure.Error.Message)
	}
	return json.Unmarshal(raw, out)
}

// wait spaces requests under the quota. Same shape as the MusicBrainz limiter.
func (d *Deezer) wait(ctx context.Context) error {
	d.mu.Lock()
	now := time.Now()
	slot := d.next
	if slot.Before(now) {
		slot = now
	}
	d.next = slot.Add(d.rate)
	d.mu.Unlock()
	delay := time.Until(slot)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
