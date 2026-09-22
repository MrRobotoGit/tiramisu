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

// ReleaseGroup is the album-level identity the Library API stores for a projection.
type ReleaseGroup struct {
	ID     string
	Artist string
	Title  string
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

// ReleaseGroupForRelease follows the release Plex stored to its release group.
func (m *MusicBrainz) ReleaseGroupForRelease(ctx context.Context, releaseID string) (ReleaseGroup, bool, error) {
	if strings.TrimSpace(releaseID) == "" {
		return ReleaseGroup{}, false, nil
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
	}
	err := m.get(ctx, "/release/"+url.PathEscape(releaseID), url.Values{"inc": {"release-groups+artist-credits"}}, &release)
	if errors.Is(err, errMusicBrainzNotFound) {
		return ReleaseGroup{}, false, nil
	}
	if err != nil {
		return ReleaseGroup{}, false, err
	}
	if release.ReleaseGroup.ID == "" {
		return ReleaseGroup{}, false, nil
	}
	return ReleaseGroup{
		ID:     release.ReleaseGroup.ID,
		Title:  release.ReleaseGroup.Title,
		Artist: strings.Join(artistNames(release.ArtistCredit), ", "),
	}, true, nil
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

func (m *MusicBrainz) get(ctx context.Context, path string, query url.Values, out interface{}) error {
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
