package engines

import (
	"regexp"
	"strings"
)

// neverMatch matches nothing (a word-boundary and a non-word-boundary can
// never both hold at the same position); used when a language list is
// emptied out via config (e.g. user clears all excluded flags).
var neverMatch = regexp.MustCompile(`\b\B`)

// FlagEmoji converts a 2-letter ISO 3166-1 alpha-2 country code into its
// Unicode flag emoji via the Regional Indicator Symbol formula — no
// name-to-emoji lookup table needed.
func FlagEmoji(code string) string {
	code = strings.ToUpper(code)
	var b strings.Builder
	for _, r := range code {
		b.WriteRune(0x1F1E6 + (r - 'A'))
	}
	return b.String()
}

// CompileLanguageRegex builds a single case-insensitive regex matching any
// of the given text terms (word-boundary wrapped) or flag emoji (derived
// from ISO country codes). Returns a never-matching regex if both lists
// are empty, since an empty alternation would otherwise match everything.
func CompileLanguageRegex(terms, flagCodes []string) *regexp.Regexp {
	parts := make([]string, 0, len(terms)+len(flagCodes))
	for _, t := range terms {
		if t == "" {
			continue
		}
		parts = append(parts, `\b`+regexp.QuoteMeta(t)+`\b`)
	}
	for _, code := range flagCodes {
		if code == "" {
			continue
		}
		parts = append(parts, regexp.QuoteMeta(FlagEmoji(code)))
	}
	if len(parts) == 0 {
		return neverMatch
	}
	return regexp.MustCompile(`(?i)` + strings.Join(parts, "|"))
}

// countryLanguage holds, for one ISO 3166-1 country code, the TMDB
// original_language codes spoken there (tmdbCodes) and the release-title
// words that commonly signal that language in scene naming (titleTerms).
type countryLanguage struct {
	tmdbCodes  []string
	titleTerms []string
}

// excludedFlagLanguages maps an ISO 3166-1 country code (as used in
// ExcludedFlags) to its language signals, single source of truth for both
// the TMDB original_language gate (discoverMovies/discoverShows) and the
// reExclLang title regex. India maps to several languages since
// Bollywood/regional cinema isn't singly-coded.
var excludedFlagLanguages = map[string]countryLanguage{
	"ES": {tmdbCodes: []string{"es"}, titleTerms: []string{"spanish", "castellano", "latino"}},
	"FR": {tmdbCodes: []string{"fr"}, titleTerms: []string{"french", "vff", "truefrench"}},
	"DE": {tmdbCodes: []string{"de"}, titleTerms: []string{"german"}},
	"RU": {tmdbCodes: []string{"ru"}, titleTerms: []string{"russian"}},
	"CN": {tmdbCodes: []string{"zh"}, titleTerms: []string{"chinese", "mandarin", "cantonese"}},
	"JP": {tmdbCodes: []string{"ja"}, titleTerms: []string{"japanese"}},
	"KR": {tmdbCodes: []string{"ko"}, titleTerms: []string{"korean"}},
	"TH": {tmdbCodes: []string{"th"}, titleTerms: []string{"thai"}},
	"PT": {tmdbCodes: []string{"pt"}, titleTerms: []string{"portuguese"}},
	"BR": {tmdbCodes: []string{"pt"}, titleTerms: []string{"brazilian"}},
	"UA": {tmdbCodes: []string{"uk"}, titleTerms: []string{"ukrainian"}},
	"PL": {tmdbCodes: []string{"pl"}, titleTerms: []string{"polish"}},
	"NL": {tmdbCodes: []string{"nl"}, titleTerms: []string{"dutch"}},
	"TR": {tmdbCodes: []string{"tr"}, titleTerms: []string{"turkish"}},
	"SA": {tmdbCodes: []string{"ar"}, titleTerms: []string{"arabic"}},
	"IN": {
		tmdbCodes:  []string{"hi", "pa", "ta", "te", "ml", "kn", "bn", "mr", "gu", "ur"},
		titleTerms: []string{"hindi", "punjabi", "tamil", "telugu", "malayalam", "kannada", "bengali", "marathi", "gujarati"},
	},
	"CZ": {tmdbCodes: []string{"cs"}, titleTerms: []string{"czech"}},
	"HU": {tmdbCodes: []string{"hu"}, titleTerms: []string{"hungarian"}},
	"RO": {tmdbCodes: []string{"ro"}, titleTerms: []string{"romanian"}},
}

// ExcludedTitleTerms expands a list of excluded flag codes into the release-
// title words associated with them, for combining with flag-emoji matching
// in CompileLanguageRegex. Defense-in-depth alongside the flag-emoji match
// (Torrentio prefixes results with flag emoji; Prowlarr/scene titles use
// plain words, when they mention language at all) — not exhaustive, since
// some releases carry no language signal in the title at all (caught
// instead by ExcludedLanguageSet's TMDB original_language gate).
func ExcludedTitleTerms(flagCodes []string) []string {
	var terms []string
	for _, code := range flagCodes {
		terms = append(terms, excludedFlagLanguages[strings.ToUpper(code)].titleTerms...)
	}
	return terms
}

// ExcludedLanguageSet expands a list of excluded flag codes into the set of
// TMDB original_language codes to reject at discovery time.
func ExcludedLanguageSet(flagCodes []string) map[string]bool {
	set := make(map[string]bool)
	for _, code := range flagCodes {
		for _, lang := range excludedFlagLanguages[strings.ToUpper(code)].tmdbCodes {
			set[lang] = true
		}
	}
	return set
}
