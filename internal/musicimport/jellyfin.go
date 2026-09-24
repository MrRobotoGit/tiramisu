package musicimport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JellyfinClient reads music libraries from a Jellyfin server. It uses only routes
// that 12.x keeps undeprecated (/Library/VirtualFolders and /Items) and the standard
// Authorization header: /Artists/AlbumArtists is obsolete in 12.0, so artists are
// read from the albums. Jellyfin keeps no play history, so it has no History method.
type JellyfinClient struct {
	baseURL string
	token   string
	http    *http.Client

	mu     sync.Mutex
	albums map[string][]jellyfinAlbum // per library, fetched once per run
}

func NewJellyfinClient(baseURL, token string) *JellyfinClient {
	return &JellyfinClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 120 * time.Second},
		albums:  map[string][]jellyfinAlbum{},
	}
}

type jellyfinAlbum struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	AlbumArtist  string `json:"AlbumArtist"`
	AlbumArtists []struct {
		Name string `json:"Name"`
	} `json:"AlbumArtists"`
	ProductionYear int               `json:"ProductionYear"`
	ProviderIDs    map[string]string `json:"ProviderIds"`
}

// artist is the first album artist: AlbumArtist joins several names in one string.
func (a jellyfinAlbum) artist() string {
	if len(a.AlbumArtists) > 0 && strings.TrimSpace(a.AlbumArtists[0].Name) != "" {
		return strings.TrimSpace(a.AlbumArtists[0].Name)
	}
	return strings.TrimSpace(a.AlbumArtist)
}

// ArtistSections lists the music libraries.
func (j *JellyfinClient) ArtistSections(ctx context.Context) ([]Section, error) {
	var folders []struct {
		Name           string `json:"Name"`
		CollectionType string `json:"CollectionType"`
		ItemID         string `json:"ItemId"`
	}
	if err := j.get(ctx, "/Library/VirtualFolders", nil, &folders); err != nil {
		return nil, err
	}
	var sections []Section
	for _, f := range folders {
		if strings.EqualFold(f.CollectionType, "music") && f.ItemID != "" {
			sections = append(sections, Section{Key: f.ItemID, Title: f.Name})
		}
	}
	return sections, nil
}

// Albums lists every album of a music library with the MusicBrainz ids Jellyfin holds.
func (j *JellyfinClient) Albums(ctx context.Context, section string) ([]Album, error) {
	raw, err := j.libraryAlbums(ctx, section)
	if err != nil {
		return nil, err
	}
	albums := make([]Album, 0, len(raw))
	for _, a := range raw {
		albums = append(albums, Album{
			RatingKey:      a.ID,
			Artist:         a.artist(),
			Title:          strings.TrimSpace(a.Name),
			Year:           a.ProductionYear,
			ReleaseID:      firstMBID(a.ProviderIDs["MusicBrainzAlbum"]),
			ReleaseGroupID: firstMBID(a.ProviderIDs["MusicBrainzReleaseGroup"]),
		})
	}
	return albums, nil
}

// Artists lists the album artists of a music library, one per name, with the first
// MusicBrainz id an album of theirs carries.
func (j *JellyfinClient) Artists(ctx context.Context, section string) ([]Artist, error) {
	raw, err := j.libraryAlbums(ctx, section)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	var artists []Artist
	for _, a := range raw {
		name := a.artist()
		if name == "" {
			continue
		}
		mbid := firstMBID(a.ProviderIDs["MusicBrainzAlbumArtist"])
		if i, ok := index[name]; ok {
			if artists[i].MBID == "" {
				artists[i].MBID = mbid
			}
			continue
		}
		index[name] = len(artists)
		artists = append(artists, Artist{Name: name, MBID: mbid})
	}
	return artists, nil
}

// jellyfinPage is how many albums one /Items request asks for.
const jellyfinPage = 500

func (j *JellyfinClient) libraryAlbums(ctx context.Context, section string) ([]jellyfinAlbum, error) {
	j.mu.Lock()
	cached, ok := j.albums[section]
	j.mu.Unlock()
	if ok {
		return cached, nil
	}
	var all []jellyfinAlbum
	for {
		query := url.Values{
			"ParentId":         {section},
			"IncludeItemTypes": {"MusicAlbum"},
			"Recursive":        {"true"},
			"Fields":           {"ProviderIds"},
			"EnableImages":     {"false"},
			"EnableUserData":   {"false"},
			"StartIndex":       {strconv.Itoa(len(all))},
			"Limit":            {strconv.Itoa(jellyfinPage)},
		}
		var page struct {
			Items []jellyfinAlbum `json:"Items"`
			Total int             `json:"TotalRecordCount"`
		}
		if err := j.get(ctx, "/Items", query, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if len(page.Items) == 0 || len(all) >= page.Total {
			break
		}
	}
	j.mu.Lock()
	j.albums[section] = all
	j.mu.Unlock()
	return all, nil
}

// firstMBID takes the first id of a provider value: a tag with several artists holds
// several ids in one field.
func firstMBID(value string) string {
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == ';' || r == ',' || r == ' ' }) {
		if part != "" {
			return part
		}
	}
	return ""
}

func (j *JellyfinClient) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	target := j.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	// 12.0 disabled X-Emby-Token and the /emby prefix; this scheme works on 10.x too.
	request.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", j.token))
	request.Header.Set("Accept", "application/json")
	response, err := j.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("jellyfin %s: status %d", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("jellyfin %s: decode: %w", path, err)
	}
	return nil
}
