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

// lbRadioEntry is one recommended recording as the endpoint returns it.
type lbRadioEntry struct {
	RecordingMBID string `json:"recording_mbid"`
	ListenCount   int    `json:"total_listen_count"`
	ArtistName    string `json:"similar_artist_name"`
}

// LBTrack is one recommended recording, flattened out of the per-artist map.
type LBTrack struct {
	RecordingMBID string
	ArtistMBID    string
	ArtistName    string
	ListenCount   int
}

// RadioOptions mirror the lb-radio/artist query parameters. Mode is not a quality
// knob: it is how far down the similarity ranking the endpoint looks (easy = nearest).
type RadioOptions struct {
	Mode                   string
	MaxSimilarArtists      int
	MaxRecordingsPerArtist int
	PopBegin               int
	PopEnd                 int
}

// ListenBrainz reads the public recommendation endpoints. Reads need no token; the
// API requires a User-Agent and no more than one request per second.
type ListenBrainz struct {
	baseURL string
	userAg  string
	rate    time.Duration
	http    *http.Client

	retryDelay time.Duration

	mu   sync.Mutex
	next time.Time
}

func NewListenBrainz() *ListenBrainz {
	return &ListenBrainz{
		baseURL: "https://api.listenbrainz.org",
		userAg:  "tiramisu-musicimport/1.0 (https://github.com/MrRobotoGit/tiramisu)",
		rate:    1100 * time.Millisecond,
		http:    &http.Client{Timeout: 30 * time.Second},

		retryDelay: 2 * time.Second,
	}
}

// RadioArtist asks for the recordings of the artists that sound like the seed,
// grouped by artist, and flattens the answer. The seed artist itself is included.
func (l *ListenBrainz) RadioArtist(ctx context.Context, seedMBID string, opts RadioOptions) ([]LBTrack, error) {
	if strings.TrimSpace(seedMBID) == "" {
		return nil, errors.New("listenbrainz: empty seed artist")
	}
	query := url.Values{
		"mode":                      {opts.Mode},
		"max_similar_artists":       {strconv.Itoa(opts.MaxSimilarArtists)},
		"max_recordings_per_artist": {strconv.Itoa(opts.MaxRecordingsPerArtist)},
		"pop_begin":                 {strconv.Itoa(opts.PopBegin)},
		"pop_end":                   {strconv.Itoa(opts.PopEnd)},
	}
	var result map[string][]lbRadioEntry
	if err := l.get(ctx, "/1/lb-radio/artist/"+url.PathEscape(seedMBID), query, &result); err != nil {
		return nil, err
	}
	tracks := make([]LBTrack, 0)
	for artistMBID, entries := range result {
		for _, e := range entries {
			if e.RecordingMBID == "" {
				continue
			}
			tracks = append(tracks, LBTrack{
				RecordingMBID: e.RecordingMBID,
				ArtistMBID:    artistMBID,
				ArtistName:    e.ArtistName,
				ListenCount:   e.ListenCount,
			})
		}
	}
	return tracks, nil
}

// LBRelease is one release of the sitewide fresh-releases feed.
type LBRelease struct {
	ArtistMBIDs      []string `json:"artist_mbids"`
	ArtistCredit     string   `json:"artist_credit_name"`
	ReleaseGroupMBID string   `json:"release_group_mbid"`
	ReleaseMBID      string   `json:"release_mbid"`
	ReleaseName      string   `json:"release_name"`
	ReleaseDate      string   `json:"release_date"`
	PrimaryType      string   `json:"release_group_primary_type"`
}

// FreshReleases lists every release of the last days (at most 90) on MusicBrainz, the
// whole site, not one user's: the pool the new-artist pass filters by taste.
func (l *ListenBrainz) FreshReleases(ctx context.Context, days int) ([]LBRelease, error) {
	query := url.Values{"days": {strconv.Itoa(days)}, "past": {"true"}, "future": {"false"}, "sort": {"release_date"}}
	var result struct {
		Payload struct {
			Releases []LBRelease `json:"releases"`
		} `json:"payload"`
	}
	if err := l.get(ctx, "/1/explore/fresh-releases/", query, &result); err != nil {
		return nil, err
	}
	return result.Payload.Releases, nil
}

// LBTag is one genre tag of an artist, with how many MusicBrainz users applied it.
type LBTag struct {
	Tag   string
	Count int
}

// LBArtist is the artist of a recording, with the year it was founded (0 = unknown).
type LBArtist struct {
	MBID      string
	Name      string
	BeginYear int
}

// ArtistGenres returns the genre tags of each artist (plain tags that are not genres
// are left out). The caller keeps batches small: the endpoint times out on long ones.
func (l *ListenBrainz) ArtistGenres(ctx context.Context, artistMBIDs []string) (map[string][]LBTag, error) {
	var result []struct {
		MBID string `json:"artist_mbid"`
		Tag  struct {
			Artist []struct {
				Tag   string `json:"tag"`
				Count int    `json:"count"`
				Genre string `json:"genre_mbid"`
			} `json:"artist"`
		} `json:"tag"`
	}
	query := url.Values{"artist_mbids": {strings.Join(artistMBIDs, ",")}, "inc": {"tag"}}
	if err := l.get(ctx, "/1/metadata/artist/", query, &result); err != nil {
		return nil, err
	}
	out := map[string][]LBTag{}
	for _, a := range result {
		for _, t := range a.Tag.Artist {
			if t.Genre != "" && t.Count > 0 {
				out[a.MBID] = append(out[a.MBID], LBTag{Tag: t.Tag, Count: t.Count})
			}
		}
	}
	return out, nil
}

// TagRecordings returns recordings carrying a MusicBrainz tag inside a popularity
// band. The band runs the other way from its name: 0 holds the most played
// recordings of the tag, 100 the least played.
func (l *ListenBrainz) TagRecordings(ctx context.Context, tag string, popBegin, popEnd, count int) ([]string, error) {
	var result []struct {
		ID string `json:"recording_mbid"`
	}
	query := url.Values{"tag": {tag}, "pop_begin": {strconv.Itoa(popBegin)}, "pop_end": {strconv.Itoa(popEnd)}, "count": {strconv.Itoa(count)}}
	if err := l.get(ctx, "/1/lb-radio/tags", query, &result); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(result))
	for _, r := range result {
		if r.ID != "" {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// RecordingArtists returns the first credited artist of each recording. The caller
// keeps batches small: the endpoint answers 502 on a hundred ids.
func (l *ListenBrainz) RecordingArtists(ctx context.Context, recordingMBIDs []string) (map[string]LBArtist, error) {
	var result map[string]json.RawMessage
	query := url.Values{"recording_mbids": {strings.Join(recordingMBIDs, ",")}, "inc": {"artist"}}
	if err := l.get(ctx, "/1/metadata/recording/", query, &result); err != nil {
		return nil, err
	}
	out := map[string]LBArtist{}
	for id, raw := range result {
		var rec struct {
			Artist struct {
				Artists []struct {
					MBID      string `json:"artist_mbid"`
					Name      string `json:"name"`
					BeginYear int    `json:"begin_year"`
				} `json:"artists"`
			} `json:"artist"`
		}
		if json.Unmarshal(raw, &rec) != nil || len(rec.Artist.Artists) == 0 {
			continue
		}
		a := rec.Artist.Artists[0]
		out[id] = LBArtist{MBID: a.MBID, Name: a.Name, BeginYear: a.BeginYear}
	}
	return out, nil
}

// listenBrainzRetries is how many times a throttled or busy answer is tried again.
const listenBrainzRetries = 3

var errListenBrainzBusy = errors.New("listenbrainz busy")

// get retries 429 and 5xx with a growing backoff; a 429 that names its reset window
// (X-RateLimit-Reset-In, seconds) waits that long instead.
func (l *ListenBrainz) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	var err error
	var wait time.Duration
	for attempt := 0; attempt <= listenBrainzRetries; attempt++ {
		if attempt > 0 {
			if wait <= 0 {
				wait = time.Duration(attempt) * l.retryDelay
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		wait, err = l.getOnce(ctx, path, query, out)
		if !errors.Is(err, errListenBrainzBusy) {
			return err
		}
	}
	return err
}

func (l *ListenBrainz) getOnce(ctx context.Context, path string, query url.Values, out interface{}) (time.Duration, error) {
	if err := l.wait(ctx); err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", l.userAg)
	response, err := l.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
		return 0, json.NewDecoder(response.Body).Decode(out)
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		var reset time.Duration
		if secs, err := strconv.Atoi(response.Header.Get("X-RateLimit-Reset-In")); err == nil && secs > 0 && secs <= 60 {
			reset = time.Duration(secs) * time.Second
		}
		return reset, fmt.Errorf("listenbrainz %s: status %d: %w", path, response.StatusCode, errListenBrainzBusy)
	default:
		return 0, fmt.Errorf("listenbrainz %s: status %d", path, response.StatusCode)
	}
}

// wait spaces requests at the configured rate. Same shape as the MusicBrainz limiter.
func (l *ListenBrainz) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	slot := l.next
	if slot.Before(now) {
		slot = now
	}
	l.next = slot.Add(l.rate)
	l.mu.Unlock()

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
