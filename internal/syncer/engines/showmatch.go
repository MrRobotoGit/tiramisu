package engines

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// reShowMarker finds where the show name ends and the season/episode part begins.
// Scene naming always puts the show first, so everything before the marker is the
// name the release claims to be.
var reShowMarker = regexp.MustCompile(`(?i)[\s._\-\[]s\d{1,2}\s*e?\d{0,3}\b|\bs\d{1,2}e\d{1,3}\b|\b\d{1,2}x\d{2}\b|\be\d{2,3}\b`)

var reShowYear = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// normalizeShowTitle folds a title down to what two spellings of the same show
// have in common: accents, punctuation and separators all disappear.
func normalizeShowTitle(s string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if folded, _, err := transform.String(t, s); err == nil {
		s = folded
	}
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// showPrefixFromFilename returns the show name a file claims to belong to, or ""
// when the name carries no usable prefix (a bare "E01.mkv" inside a pack).
func showPrefixFromFilename(filename string) string {
	if i := strings.LastIndexByte(filename, '/'); i >= 0 {
		filename = filename[i+1:]
	}
	if m := reShowMarker.FindStringIndex(filename); m != nil {
		filename = filename[:m[0]]
	}
	filename = reShowYear.ReplaceAllString(filename, " ")
	return normalizeShowTitle(filename)
}

// titlesMatch accepts a prefix that equals a known title or extends it with more
// words: "lucky 2026 extended" still names Lucky, "one day" does not name Day One.
func titlesMatch(prefix, known string) bool {
	if prefix == "" || known == "" {
		return false
	}
	return prefix == known ||
		strings.HasPrefix(prefix, known+" ") ||
		strings.HasPrefix(known, prefix+" ")
}

// filesMatchShow reports whether a torrent's real contents belong to the show we
// asked for. Release titles are written by whoever uploaded them; the files
// inside are what ends up in the library, so those are what we check.
// Files with no usable prefix cannot disagree, so a torrent made only of those
// is accepted rather than rejected on absent evidence.
func filesMatchShow(filenames []string, knownTitles []string) (matched, decidable bool) {
	var normKnown []string
	for _, k := range knownTitles {
		if n := normalizeShowTitle(k); n != "" {
			normKnown = append(normKnown, n)
		}
	}
	if len(normKnown) == 0 {
		return true, false
	}

	for _, f := range filenames {
		prefix := showPrefixFromFilename(f)
		if prefix == "" {
			continue
		}
		decidable = true
		for _, k := range normKnown {
			if titlesMatch(prefix, k) {
				return true, true
			}
		}
	}
	return !decidable, decidable
}
