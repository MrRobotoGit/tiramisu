package engines

import (
	"regexp"
	"strings"
	"unicode"
)

// preferredFlagTMDBLang maps a preferred-flag country code to the TMDB language tag
// whose episode names that audience's releases use. Unlisted codes are skipped.
var preferredFlagTMDBLang = map[string]string{
	"IT": "it-IT", "US": "en-US", "GB": "en-GB", "ES": "es-ES", "FR": "fr-FR",
	"DE": "de-DE", "PT": "pt-PT", "BR": "pt-BR", "NL": "nl-NL", "PL": "pl-PL",
	"RU": "ru-RU", "JP": "ja-JP", "KR": "ko-KR", "CN": "zh-CN",
}

// TMDBEpisodeLanguages returns the language tags to fetch episode names in: always
// en-US (scene names are English) plus one tag per preferred flag, deduped by base
// language so en-US and en-GB do not cost two requests.
func TMDBEpisodeLanguages(preferredFlags []string) []string {
	langs := []string{"en-US"}
	seen := map[string]bool{"en": true}
	for _, code := range preferredFlags {
		tag, ok := preferredFlagTMDBLang[strings.ToUpper(code)]
		if !ok {
			continue
		}
		base := strings.SplitN(tag, "-", 2)[0]
		if seen[base] {
			continue
		}
		seen[base] = true
		langs = append(langs, tag)
	}
	return langs
}

// reReleaseTech marks where a release title stops naming the episode and starts
// describing the file: resolution, source, codec, audio and language tags.
var reReleaseTech = regexp.MustCompile(`(?i)(?:^|[\s._\-\[])(?:` +
	`2160p|1080p|720p|480p|4k|uhd|` +
	`web|webrip|web-dl|webdl|bluray|blu-ray|bdrip|brrip|dvdrip|hdtv|pdtv|remux|repack|proper|` +
	`amzn|atvp|nf|dsnp|hmax|max|hulu|itunes|stan|pcok|` +
	`x264|x265|h264|h265|hevc|avc|av1|xvid|10bit|8bit|` +
	`ddp\d?|dd\d?|dts|truehd|atmos|aac|ac3|eac3|flac|opus|` +
	`hdr|hdr10|dv|dovi|sdr|mkv|mp4|avi|` +
	`final|internal|limited|extended|uncut|remastered|hybrid|imax|readnfo|nfo|dubbed|hardsub|` +
	`ita|eng|multi|dual|vost\w*|sub|subs|subbed` +
	`)(?:$|[\s._\-\[])`)

// reTMDBPlaceholder matches TMDB's filler names for episodes that have no real
// title yet ("Episode 10", "TBA"), which carry no evidence either way.
var reTMDBPlaceholder = regexp.MustCompile(`(?i)^(?:episode|episodio|puntata)\s*\d*$|^tba$|^tbd$`)

// reLeadingYear strips a bare year that scene naming puts between the episode
// marker and the technical tags.
var reLeadingYear = regexp.MustCompile(`^(?:19|20)\d{2}\b`)

// episodeNameFromRelease returns the episode-name segment a release carries between
// its SxxEyy marker and the first technical tag, normalized. Returns "" when the
// release carries no name — most scene releases do not.
func episodeNameFromRelease(title string) string {
	firstLine := strings.Split(title, "\n")[0]
	loc := reTVEpNum.FindStringIndex(firstLine)
	if loc == nil {
		return ""
	}
	rest := firstLine[loc[1]:]
	if m := reReleaseTech.FindStringIndex(rest); m != nil {
		rest = rest[:m[0]]
	}
	name := normalizeShowTitle(rest)
	// A bare year right after the marker is metadata, not a title ("S04E05 2020 2160p").
	name = strings.TrimSpace(reLeadingYear.ReplaceAllString(name, ""))
	// "Episode 3" and a leftover episode number carry no evidence either way.
	if name == "" || reTMDBPlaceholder.MatchString(name) || !hasWordOfLetters(name, 3) {
		return ""
	}
	return name
}

// hasWordOfLetters reports whether any word holds at least n letters. Scene noise
// left over from extraction is short and alphanumeric ("en", "6v"); a real episode
// name has at least one proper word ("One", "NPC", "Home").
func hasWordOfLetters(s string, n int) bool {
	for _, w := range strings.Fields(s) {
		letters := 0
		for _, r := range w {
			if unicode.IsLetter(r) {
				letters++
			}
		}
		if letters >= n {
			return true
		}
	}
	return false
}

// episodeNameContradicts reports whether a release names an episode that is not the
// one TMDB lists under that number. Two shows can share a title (Dark Matter 2024 and
// Dark Matter 2015), so the episode name is the only thing separating them once the
// season and episode numbers already agree. Absence of evidence is never a rejection:
// a release with no name, or a TMDB entry with none, returns false.
func episodeNameContradicts(releaseTitle string, tmdbNames []string) bool {
	claimed := episodeNameFromRelease(releaseTitle)
	if claimed == "" {
		return false
	}
	claimedWords := contentWords(claimed)
	if len(claimedWords) == 0 {
		return false
	}
	decidable := false
	for _, n := range tmdbNames {
		if reTMDBPlaceholder.MatchString(strings.TrimSpace(n)) {
			continue
		}
		known := normalizeShowTitle(n)
		if known == "" {
			continue
		}
		decidable = true
		if titlesMatch(claimed, known) {
			return false
		}
		// Releases abbreviate and reorder ("Parte Quinta" for "Quinta parte: La
		// guerriera ombra"), so equality is too strict. One shared content word is
		// enough to say the two name the same episode; a release of a different show
		// shares none ("Take the Shot" against "No Quiet Life").
		for w := range contentWords(known) {
			if claimedWords[w] {
				return false
			}
		}
	}
	return decidable
}

// episodeStopWords are the articles and prepositions that two unrelated titles
// share by chance, in the languages episode names are fetched in.
var episodeStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "and": true, "or": true,
	"in": true, "on": true, "to": true, "for": true, "with": true, "at": true,
	"by": true, "is": true, "it": true, "as": true, "from": true, "my": true,
	"il": true, "lo": true, "la": true, "i": true, "gli": true, "le": true,
	"un": true, "uno": true, "una": true, "di": true, "del": true, "della": true,
	"dei": true, "delle": true, "e": true, "ed": true, "che": true, "non": true,
	"per": true, "con": true, "su": true, "da": true, "al": true, "alla": true,
	"nel": true, "nella": true, "si": true, "se": true, "ma": true, "l": true,
}

// contentWords returns the meaningful words of a normalized title.
func contentWords(s string) map[string]bool {
	out := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		if len(w) < 2 || episodeStopWords[w] {
			continue
		}
		out[w] = true
	}
	return out
}
