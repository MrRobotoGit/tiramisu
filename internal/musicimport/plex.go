// Package musicimport rebuilds a Plex music library inside Tiramisu. It reads the
// albums Plex already knows, resolves each one to a MusicBrainz release group,
// finds a lossless torrent with acceptable seeders and files it through the
// Library API. Discovery, naming and scoring live here, never in the engine.
package musicimport

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Album is one album as Plex describes it.
type Album struct {
	RatingKey string
	Artist    string
	Title     string
	Year      int
	// ReleaseID is the MusicBrainz release id Plex stored for the album, empty
	// when the album was never matched.
	ReleaseID string
}

// Artist is one artist as Plex describes it. MBID is empty when the agent never
// matched the artist to MusicBrainz.
type Artist struct {
	Name string
	MBID string
}

// Play is one music play from the Plex history.
type Play struct {
	Artist    string
	Album     string
	Title     string
	ViewedAt  time.Time
	SectionID string
}

// PlexClient reads music libraries from a Plex server.
type PlexClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewPlexClient(baseURL, token string) *PlexClient {
	return &PlexClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

type plexGuid struct {
	ID string `xml:"id,attr"`
}

type plexSectionsContainer struct {
	XMLName  xml.Name `xml:"MediaContainer"`
	Sections []struct {
		Key   string `xml:"key,attr"`
		Type  string `xml:"type,attr"`
		Title string `xml:"title,attr"`
	} `xml:"Directory"`
}

type plexAlbumsContainer struct {
	XMLName xml.Name `xml:"MediaContainer"`
	Albums  []struct {
		RatingKey   string     `xml:"ratingKey,attr"`
		Title       string     `xml:"title,attr"`
		ParentTitle string     `xml:"parentTitle,attr"`
		Year        int        `xml:"year,attr"`
		Guids       []plexGuid `xml:"Guid"`
	} `xml:"Directory"`
}

type plexArtistsContainer struct {
	XMLName xml.Name `xml:"MediaContainer"`
	Artists []struct {
		Title string     `xml:"title,attr"`
		Guids []plexGuid `xml:"Guid"`
	} `xml:"Directory"`
}

// Plex serves history as one element per media type; only tracks matter here.
type plexHistoryContainer struct {
	XMLName xml.Name `xml:"MediaContainer"`
	Tracks  []struct {
		Title            string `xml:"title,attr"`
		GrandparentTitle string `xml:"grandparentTitle,attr"`
		ParentTitle      string `xml:"parentTitle,attr"`
		ViewedAt         int64  `xml:"viewedAt,attr"`
		LibrarySectionID string `xml:"librarySectionID,attr"`
	} `xml:"Track"`
}

// ArtistSections lists the artist-type library sections, in Plex order.
func (p *PlexClient) ArtistSections(ctx context.Context) ([]Section, error) {
	var container plexSectionsContainer
	if err := p.get(ctx, "/library/sections", nil, &container); err != nil {
		return nil, err
	}
	var sections []Section
	for _, s := range container.Sections {
		if s.Type == "artist" {
			sections = append(sections, Section{Key: s.Key, Title: s.Title})
		}
	}
	return sections, nil
}

// Section is one Plex library section.
type Section struct {
	Key   string
	Title string
}

// Albums lists every album of an artist-type section.
func (p *PlexClient) Albums(ctx context.Context, section string) ([]Album, error) {
	var container plexAlbumsContainer
	query := url.Values{"includeGuids": {"1"}}
	path := "/library/sections/" + url.PathEscape(section) + "/albums"
	if err := p.get(ctx, path, query, &container); err != nil {
		return nil, err
	}
	albums := make([]Album, 0, len(container.Albums))
	for _, a := range container.Albums {
		albums = append(albums, Album{
			RatingKey: a.RatingKey,
			Artist:    strings.TrimSpace(a.ParentTitle),
			Title:     strings.TrimSpace(a.Title),
			Year:      a.Year,
			ReleaseID: plexReleaseID(a.Guids),
		})
	}
	return albums, nil
}

// plexReleaseID picks the MusicBrainz id from an album's GUID list. Plex stores the
// release id, not the release group; resolution to a group happens on MusicBrainz.
func plexReleaseID(guids []plexGuid) string {
	for _, g := range guids {
		if strings.HasPrefix(g.ID, "mbid://") {
			return strings.TrimPrefix(g.ID, "mbid://")
		}
	}
	return ""
}

// Artists lists every artist of an artist-type section, with the MusicBrainz id the
// agent stored (includeGuids=1 surfaces it as an mbid:// GUID).
func (p *PlexClient) Artists(ctx context.Context, section string) ([]Artist, error) {
	var container plexArtistsContainer
	query := url.Values{"includeGuids": {"1"}, "type": {"8"}}
	path := "/library/sections/" + url.PathEscape(section) + "/all"
	if err := p.get(ctx, path, query, &container); err != nil {
		return nil, err
	}
	artists := make([]Artist, 0, len(container.Artists))
	for _, a := range container.Artists {
		name := strings.TrimSpace(a.Title)
		if name == "" {
			continue
		}
		artists = append(artists, Artist{Name: name, MBID: plexReleaseID(a.Guids)})
	}
	return artists, nil
}

// History lists the music plays that started after the given time; a zero time means
// all of them. The seed pass reads this once per window.
func (p *PlexClient) History(ctx context.Context, after time.Time) ([]Play, error) {
	var container plexHistoryContainer
	query := url.Values{"sort": {"viewedAt:desc"}}
	if !after.IsZero() {
		query.Set("viewedAt>", strconv.FormatInt(after.Unix(), 10))
	}
	if err := p.get(ctx, "/status/sessions/history/all", query, &container); err != nil {
		return nil, err
	}
	plays := make([]Play, 0, len(container.Tracks))
	for _, t := range container.Tracks {
		if strings.TrimSpace(t.GrandparentTitle) == "" {
			continue
		}
		plays = append(plays, Play{
			Artist:    strings.TrimSpace(t.GrandparentTitle),
			Album:     strings.TrimSpace(t.ParentTitle),
			Title:     strings.TrimSpace(t.Title),
			ViewedAt:  time.Unix(t.ViewedAt, 0),
			SectionID: t.LibrarySectionID,
		})
	}
	return plays, nil
}

func (p *PlexClient) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return err
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("X-Plex-Token", p.token)
	request.URL.RawQuery = query.Encode()
	response, err := p.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("plex %s: status %d", path, response.StatusCode)
	}
	if err := xml.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("plex %s: decode: %w", path, err)
	}
	return nil
}
