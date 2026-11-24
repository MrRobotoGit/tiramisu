package library

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reTitleHash8 = regexp.MustCompile(`(?i)_[a-f0-9]{8}$`)
	reTitleRes   = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p)\b`)
	reTitleYears = regexp.MustCompile(`\b(?:19|20)\d\d\b`)
)

// TitleFromFilename extracts a clean display title from an MKV filename.
// e.g. "Your_Friends_Neighbors_S01E09_17361ba1.mkv" → "Your Friends Neighbors S01E09"
func TitleFromFilename(filename string) string {
	s := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Remove trailing _hash8 (8 hex chars)
	if reTitleHash8.MatchString(s) {
		s = s[:len(s)-9]
	}
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	return strings.TrimSpace(s)
}

// DisplayTitle splits a stub name into "Title (year)" or "Show SxxEyy", and its resolution.
func DisplayTitle(filename string) (title, resolution string) {
	s := TitleFromFilename(filename)
	if m := reTitleRes.FindStringSubmatch(s); m != nil {
		resolution = strings.ToLower(m[1])
	}
	if ep := reEpSxxExx.FindStringIndex(s); ep != nil {
		return strings.TrimSpace(s[:ep[1]]), resolution
	}
	// The year closest before the resolution, or the last one: a title can hold a year of its own.
	cut := s
	if i := reTitleRes.FindStringIndex(s); i != nil {
		cut = strings.TrimSpace(s[:i[0]])
	}
	years := reTitleYears.FindAllStringIndex(cut, -1)
	if len(years) > 0 {
		y := years[len(years)-1]
		if y[0] > 0 {
			return strings.TrimSpace(cut[:y[0]]) + " (" + cut[y[0]:y[1]] + ")", resolution
		}
	}
	return cut, resolution
}
