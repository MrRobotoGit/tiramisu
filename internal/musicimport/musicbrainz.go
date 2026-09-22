package musicimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errMusicBrainzNotFound is a 404 answer, which for this pipeline is a normal
// outcome (a stale Plex id) rather than a failure.
var errMusicBrainzNotFound = errors.New("musicbrainz: not found")

// errMusicBrainzBusy marks an answer the server may give differently a moment later.
var errMusicBrainzBusy = errors.New("musicbrainz: temporarily unavailable")

// ReleaseGroup is the album-level identity the Library API stores for a projection.
type ReleaseGroup struct {
	ID     string
	Artist string
	Title  string
}

// ReleaseTrack is one entry of a release's tracklist. Plex stores and sends the
// track id, not the recording id: MusicBrainz has no lookup endpoint for it, but the
// release's own tracklist is where it lives.
type ReleaseTrack struct {
	ID        string // MusicBrainz track id, the one Plex speaks
	Recording string
	Medium    int
	Position  string // the track's number inside its medium, verbatim
	Title     string
}

// MusicBrainz resolves release ids and searches release groups. Its requests are
// spaced at MusicBrainz's one per second, cached by the caller's state file.
type MusicBrainz struct {
	baseURL string
	userAg  string
	rate    time.Duration
	http    *http.Client

	mu   sync.Mutex
	next time.Time
}

func NewMusicBrainz() *MusicBrainz {
	return &MusicBrainz{
		baseURL: "https://musicbrainz.org/ws/2",
		userAg:  "tiramisu-musicimport/1.0 (https://github.com/MrRobotoGit/tiramisu)",
		rate:    1100 * time.Millisecond,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ReleaseDetails follows the release Plex stored to its release group and reads its
// tracklist in the same call. The tracklist is the only place a track id resolves.
func (m *MusicBrainz) ReleaseDetails(ctx context.Context, releaseID string) (ReleaseGroup, []ReleaseTrack, bool, error) {
	if strings.TrimSpace(releaseID) == "" {
		return ReleaseGroup{}, nil, false, nil
	}
	var release struct {
		Title        string `json:"title"`
		ReleaseGroup struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"release-group"`
		ArtistCredit []struct {
			Name string `json:"name"`
		} `json:"artist-credit"`
		Media []struct {
			Position int `json:"position"`
			Tracks   []struct {
				ID        string `json:"id"`
				Number    string `json:"number"`
				Title     string `json:"title"`
				Recording struct {
					ID string `json:"id"`
				} `json:"recording"`
			} `json:"tracks"`
		} `json:"media"`
	}
	err := m.get(ctx, "/release/"+url.PathEscape(releaseID), url.Values{"inc": {"release-groups+artist-credits+recordings"}}, &release)
	if errors.Is(err, errMusicBrainzNotFound) {
		return ReleaseGroup{}, nil, false, nil
	}
	if err != nil {
		return ReleaseGroup{}, nil, false, err
	}
	if release.ReleaseGroup.ID == "" {
		return ReleaseGroup{}, nil, false, nil
	}
	var tracks []ReleaseTrack
	for _, medium := range release.Media {
		for _, track := range medium.Tracks {
			tracks = append(tracks, ReleaseTrack{
				ID:        track.ID,
				Recording: track.Recording.ID,
				Medium:    medium.Position,
				Position:  track.Number,
				Title:     track.Title,
			})
		}
	}
	return ReleaseGroup{
		ID:     release.ReleaseGroup.ID,
		Title:  release.ReleaseGroup.Title,
		Artist: strings.Join(artistNames(release.ArtistCredit), ", "),
	}, tracks, true, nil
}

// SearchReleaseGroup looks a release group up by artist and title, for albums Plex
// never matched or whose stored id no longer resolves.
func (m *MusicBrainz) SearchReleaseGroup(ctx context.Context, artist, title string) (ReleaseGroup, bool, error) {
	if strings.TrimSpace(title) == "" {
		return ReleaseGroup{}, false, nil
	}
	query := fmt.Sprintf(`releasegroup:"%s"`, strings.ReplaceAll(title, `"`, " "))
	if strings.TrimSpace(artist) != "" {
		query += fmt.Sprintf(` AND artist:"%s"`, strings.ReplaceAll(artist, `"`, " "))
	}
	var result struct {
		ReleaseGroups []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Score int    `json:"score"`
			AC    []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"release-groups"`
	}
	if err := m.get(ctx, "/release-group", url.Values{"query": {query}, "limit": {"5"}}, &result); err != nil {
		return ReleaseGroup{}, false, err
	}
	best := ReleaseGroup{}
	bestScore := 0
	for _, rg := range result.ReleaseGroups {
		if rg.ID == "" {
			continue
		}
		// Prefer an exact title match over a fuzzier, higher-scored suggestion.
		score := rg.Score
		if strings.EqualFold(strings.TrimSpace(rg.Title), strings.TrimSpace(title)) {
			score += 100
		}
		if score > bestScore {
			bestScore = score
			best = ReleaseGroup{
				ID:     rg.ID,
				Title:  rg.Title,
				Artist: strings.Join(artistNames(rg.AC), ", "),
			}
		}
	}
	return best, best.ID != "", nil
}

func artistNames(credit []struct {
	Name string `json:"name"`
}) []string {
	names := make([]string, 0, len(credit))
	for _, ac := range credit {
		if name := strings.TrimSpace(ac.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// musicBrainzRetries is how many times a throttled or unavailable answer is tried
// again. MusicBrainz returns 503 under load and 429 when the rate is exceeded, both
// transient: without a retry a whole album is dropped for a reason that has nothing
// to do with the match, and over a few thousand albums that loss adds up.
const musicBrainzRetries = 3

func (m *MusicBrainz) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	var err error
	for attempt := 0; attempt <= musicBrainzRetries; attempt++ {
		if attempt > 0 {
			// Backs off on top of the rate limiter's own spacing: a server already
			// saying "too fast" is not helped by arriving on schedule.
			delay := time.Duration(attempt) * time.Second
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		err = m.getOnce(ctx, path, query, out)
		if !isTransientMusicBrainz(err) {
			return err
		}
	}
	return err
}

// isTransientMusicBrainz reports whether the answer is worth asking for again.
func isTransientMusicBrainz(err error) bool {
	return err != nil && (errors.Is(err, errMusicBrainzBusy))
}

func (m *MusicBrainz) getOnce(ctx context.Context, path string, query url.Values, out interface{}) error {
	if err := m.wait(ctx); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", m.userAg)
	request.Header.Set("Accept", "application/json")
	response, err := m.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(response.Body).Decode(out)
	case http.StatusNotFound:
		return errMusicBrainzNotFound
	case http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("musicbrainz %s: status %d: %w", path, response.StatusCode, errMusicBrainzBusy)
	default:
		return fmt.Errorf("musicbrainz %s: status %d", path, response.StatusCode)
	}
}

// wait spaces requests at the configured rate, the way the API terms require.
func (m *MusicBrainz) wait(ctx context.Context) error {
	m.mu.Lock()
	now := time.Now()
	slot := m.next
	if slot.Before(now) {
		slot = now
	}
	m.next = slot.Add(m.rate)
	m.mu.Unlock()

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
