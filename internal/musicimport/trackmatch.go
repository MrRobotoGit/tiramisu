package musicimport

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	reDiscDir     = regexp.MustCompile(`(?i)(?:disc|cd)[\s._-]*(\d{1,2})`)
	rePairNum     = regexp.MustCompile(`^(\d{1,2})[-._](\d{1,3})[\s._-]`)
	reTrackNum    = regexp.MustCompile(`^(\d{1,3})[\s._-]`)
	reLeading     = regexp.MustCompile(`^\d{1,3}[-._\s]+`)
	reLeadingPair = regexp.MustCompile(`^\d{1,2}[-._]\d{1,3}[-._\s]+`)
)

// matchTrack maps a release file to its tracklist entry. Plex identifies a track by
// the MusicBrainz track id, and the release's tracklist is the only place that id
// exists, so a projection added without it can never match a Plex webhook.
//
// Number first because torrent files almost always start with one, title tokens as
// the fallback for layouts that do not (or for a tracklist whose numbering differs).
func matchTrack(sourcePath string, tracks []ReleaseTrack) (ReleaseTrack, bool) {
	if len(tracks) == 0 {
		return ReleaseTrack{}, false
	}
	disc, number := trackNumbers(sourcePath)
	if number > 0 {
		// A disc number in the path pins the medium; without one, track 1 of any
		// medium is a coin toss, so medium 1 wins before the first match does.
		var fallback ReleaseTrack
		haveFallback := false
		for _, track := range tracks {
			position, ok := numericPosition(track.Position)
			if !ok || position != number {
				continue
			}
			if disc > 0 && track.Medium != disc {
				continue
			}
			// A number alone can match the same position of another medium (flat
			// multi-disc torrents) or a mislabelled file. If the name carries title
			// words and shares none with the candidate, the number pointed elsewhere:
			// no id is better than a wrong one, the caller falls back to the album.
			if titleContradicts(sourcePath, track.Title) {
				continue
			}
			if disc > 0 || track.Medium == 1 {
				return track, true
			}
			if !haveFallback {
				fallback, haveFallback = track, true
			}
		}
		if haveFallback {
			return fallback, true
		}
	}
	return matchTrackByTitle(sourcePath, tracks)
}

// trackNumbers reads the disc and track numbers a file name carries: "Disc 2/01 - X",
// "2-01 - X", or just "01 - X".
func trackNumbers(sourcePath string) (disc, number int) {
	base := sourcePath
	dir := ""
	if cut := strings.LastIndexByte(sourcePath, '/'); cut >= 0 {
		base, dir = sourcePath[cut+1:], sourcePath[:cut]
	}
	if match := reDiscDir.FindStringSubmatch(dir); match != nil {
		disc, _ = strconv.Atoi(match[1])
	}
	if match := rePairNum.FindStringSubmatch(base); match != nil {
		if disc == 0 {
			disc, _ = strconv.Atoi(match[1])
		}
		number, _ = strconv.Atoi(match[2])
		return disc, number
	}
	if match := reTrackNum.FindStringSubmatch(base); match != nil {
		number, _ = strconv.Atoi(match[1])
	}
	return disc, number
}

func numericPosition(position string) (int, bool) {
	position = strings.TrimSpace(position)
	if position == "" {
		return 0, false
	}
	value, err := strconv.Atoi(position)
	if err != nil {
		return 0, false
	}
	return value, true
}

// matchTrackByTitle token-matches the file name against the tracklist, requiring all
// tokens of the shorter side to appear in the longer one. Ambiguity returns no match
// rather than a guess: the caller then keeps the album identity, which is safe.
func matchTrackByTitle(sourcePath string, tracks []ReleaseTrack) (ReleaseTrack, bool) {
	fileTokens := fileTitleTokens(sourcePath)
	if len(fileTokens) == 0 {
		return ReleaseTrack{}, false
	}

	best := ReleaseTrack{}
	bestScore := 0
	ambiguous := false
	for _, track := range tracks {
		trackTokens := normalizeTokens(track.Title)
		if len(trackTokens) == 0 {
			continue
		}
		matched := 0
		for _, token := range fileTokens {
			if containsToken(trackTokens, token) {
				matched++
			}
		}
		required := len(fileTokens)
		if len(trackTokens) < required {
			required = len(trackTokens)
		}
		if matched < required {
			continue
		}
		if matched > bestScore {
			best, bestScore, ambiguous = track, matched, false
		} else if matched == bestScore {
			ambiguous = true
		}
	}
	if bestScore == 0 || ambiguous {
		return ReleaseTrack{}, false
	}
	return best, true
}

func containsToken(tokens []string, token string) bool {
	for _, candidate := range tokens {
		if candidate == token {
			return true
		}
	}
	return false
}

// fileTitleTokens is the file name's own words: extension, track numbers and disc
// markers stripped, so "2-01 - Faith Alone.flac" yields [faith, alone].
func fileTitleTokens(sourcePath string) []string {
	base := sourcePath
	if cut := strings.LastIndexByte(base, '/'); cut >= 0 {
		base = base[cut+1:]
	}
	if cut := strings.LastIndexByte(base, '.'); cut > 0 {
		base = base[:cut]
	}
	base = reLeadingPair.ReplaceAllString(base, "")
	base = reLeading.ReplaceAllString(base, "")
	return normalizeTokens(base)
}

// titleContradicts reports whether a file name and a track title share no word at
// all. An empty side never contradicts: many torrent files carry no title.
func titleContradicts(sourcePath, trackTitle string) bool {
	fileTokens := fileTitleTokens(sourcePath)
	trackTokens := normalizeTokens(trackTitle)
	if len(fileTokens) == 0 || len(trackTokens) == 0 {
		return false
	}
	for _, token := range fileTokens {
		if containsToken(trackTokens, token) {
			return false
		}
	}
	return true
}
