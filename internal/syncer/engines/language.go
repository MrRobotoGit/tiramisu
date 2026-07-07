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
