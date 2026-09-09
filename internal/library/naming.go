// Package library holds the stub-file format and filename conventions shared by the
// sync engines and by the external /api/library endpoints. Both must agree: a stub
// created through the API under a different name would be a duplicate of the same
// title on disk, and a TV episode filed under a different registry key is deleted by
// the next sync as orphaned.
package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	reHDR       = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])hdr(?:$|[^A-Za-z0-9])|hdr10\+?`)
	reDV        = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])dv(?:$|[^A-Za-z0-9])|dovi|dolby.?vision`)
	reAtmos     = regexp.MustCompile(`(?i)atmos`)
	re51        = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	reRemux     = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])remux(?:$|[^A-Za-z0-9])`)
	reTitleYear = regexp.MustCompile(`(.+?)[._\s]\(?((?:19|20)\d{2})\)?`)

	reMovieUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	reUnderscores = regexp.MustCompile(`_+`)
	reShowUnsafe  = regexp.MustCompile(`[<>:"/\\|?*'"&]`)
	reSpaces      = regexp.MustCompile(`\s+`)
	reNonWord     = regexp.MustCompile(`[^a-z0-9]`)
	reEpSxxExx    = regexp.MustCompile(`[Ss](\d+)[Ee](\d+)`)
	reEpNxN       = regexp.MustCompile(`(\d+)x(\d+)`)
)

// MovieName is everything the movie filename is built from. ReleaseTitle is the raw
// release name the quality tags are read out of; Hash is the full info hash.
type MovieName struct {
	Title        string
	ReleaseDate  string
	ReleaseTitle string
	Is4K         bool
	Hash         string
}

func SanitizeMovieName(s string) string {
	s = reMovieUnsafe.ReplaceAllString(s, "_")
	s = reUnderscores.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// BuildMovieFilename returns the stub name for a movie. The trailing 8 chars are the
// tail of the info hash: that suffix is how an already-present title is recognised.
func BuildMovieFilename(n MovieName) string {
	year := ""
	if len(n.ReleaseDate) >= 4 {
		year = n.ReleaseDate[:4]
	} else if m := reTitleYear.FindStringSubmatch(n.Title); len(m) > 2 {
		year = m[2]
	}

	base := SanitizeMovieName(n.Title)
	if year != "" {
		base = fmt.Sprintf("%s_%s", base, year)
	}

	if n.Is4K {
		base += "_2160p"
	} else {
		base += "_1080p"
	}

	if reDV.MatchString(n.ReleaseTitle) {
		base += "_DV"
	} else if reHDR.MatchString(n.ReleaseTitle) {
		base += "_HDR"
	}

	if reAtmos.MatchString(n.ReleaseTitle) {
		base += "_Atmos"
	} else if re51.MatchString(n.ReleaseTitle) {
		base += "_5.1"
	}

	if reRemux.MatchString(n.ReleaseTitle) {
		base += "_REMUX"
	}

	return fmt.Sprintf("%s_%s.mkv", base, HashSuffix(n.Hash))
}

// HashSuffix is the last 8 hash chars used in movie filenames.
func HashSuffix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[len(hash)-8:]
}

// SanitizeShowName turns a show name into one safe directory component. Titles reach
// here from HTTP requests, so a name made of dots must not survive as "." or "..".
func SanitizeShowName(name string) string {
	clean := reShowUnsafe.ReplaceAllString(name, "")
	clean = reSpaces.ReplaceAllString(clean, "_")
	clean = reUnderscores.ReplaceAllString(clean, "_")
	clean = strings.Trim(clean, "_")
	clean = strings.TrimLeft(clean, ".")
	if clean == "" {
		return "untitled"
	}
	return clean
}

func ShowFolderName(showName, firstAirDate string) string {
	clean := SanitizeShowName(showName)
	if len(firstAirDate) >= 4 {
		return fmt.Sprintf("%s (%s)", clean, firstAirDate[:4])
	}
	return clean
}

func EpisodeFilename(show string, season, episode int, hash8 string) string {
	return fmt.Sprintf("%s_S%02dE%02d_%s.mkv", SanitizeShowName(show), season, episode, hash8)
}

// EpisodeKey is the TV registry key. The sync deletes every stub under the TV dir whose
// path is not registered, so an API-created episode must use this exact form.
func EpisodeKey(show string, season, episode int) string {
	return fmt.Sprintf("%s_s%02de%02d", reNonWord.ReplaceAllString(strings.ToLower(show), ""), season, episode)
}

// ParseSeasonEpisode reads S01E05 or 1x05 out of a filename, returning 0,0 when the
// name carries neither.
func ParseSeasonEpisode(filename string) (int, int) {
	for _, re := range []*regexp.Regexp{reEpSxxExx, reEpNxN} {
		if m := re.FindStringSubmatch(filename); len(m) >= 3 {
			season, _ := strconv.Atoi(m[1])
			episode, _ := strconv.Atoi(m[2])
			return season, episode
		}
	}
	return 0, 0
}

func IsVideoFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".avi", ".mov", ".m4v":
		return true
	}
	return false
}

// WriteStub writes the virtual .mkv: a small JSON file the FUSE layer exposes at the
// declared size.
func WriteStub(path, streamURL string, size int64, magnet, imdbID string) error {
	data, err := json.Marshal(map[string]interface{}{
		"url":    streamURL,
		"size":   size,
		"magnet": magnet,
		"imdb":   imdbID,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
