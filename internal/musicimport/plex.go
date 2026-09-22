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
