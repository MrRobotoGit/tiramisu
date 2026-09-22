package musicimport

import (
	"sort"
	"strings"

	"tiramisu/internal/prowlarr"
)

// Candidate is one scored torrent for an album. Hash may be empty when the indexer
// exposes only a download link, which the caller resolves before filing it.
type Candidate struct {
	Title       string
	Hash        string
	DownloadURL string
	Seeders     int
	Size        int64
	Score       int
}

const minAlbumBytes = 15 << 20 // an album below 15 MB is not a lossless release

// SelectCandidate returns the best lossless torrent for an album, preferring one
// whose hash is already known. ok is false when nothing qualifies: Phase 1 files
// FLAC only, so an MP3-only album is left for a later pass instead of being
// imported in a worse format, and a torrent the swarm has abandoned is not worth
// a projection.
func SelectCandidate(results []prowlarr.ProwlarrResult, artist, album string, minSeeders int, maxSizeBytes int64) (Candidate, bool) {
	candidates := SelectCandidates(results, artist, album, minSeeders, maxSizeBytes, 5)
	for _, candidate := range candidates {
		if candidate.Hash != "" {
			return candidate, true
		}
	}
	if len(candidates) > 0 {
		return candidates[0], true
	}
	return Candidate{}, false
}

// SelectCandidates scores and ranks every qualifying result, best first, so a
// caller can fall back to the next one when a hash cannot be resolved.
func SelectCandidates(results []prowlarr.ProwlarrResult, artist, album string, minSeeders int, maxSizeBytes int64, limit int) []Candidate {
	albumTokens := normalizeTokens(album)
	artistTokens := normalizeTokens(artist)

	// A title that yields no comparable token cannot be told apart from any other
	// release by the same artist, and the checks below would wave every one of them
	// through: "len(albumTokens) > 0" guards the album filter, and the exact-match
	// bonus reads as satisfied when both sides are zero. The Greenhornes' "★★★★"
	// filed a 1999 album under the 2010 name that way. Symbols, short numerals and
	// stopword-only titles all land here, so refusing is the only safe answer: an
	// album nobody can verify is one to add by hand, not to guess.
	if strings.TrimSpace(album) != "" && len(albumTokens) == 0 {
		return nil
	}

	var candidates []Candidate
	for _, result := range results {
		hash := strings.ToLower(strings.TrimSpace(result.InfoHash))
		link := strings.TrimSpace(result.DownloadUrl)
		if hash == "" && link == "" {
			continue
		}
		title := normalizeTitle(result.Title)
		if !strings.Contains(title, " flac ") && !strings.HasSuffix(title, " flac") {
			continue
		}
		if result.Seeders < minSeeders {
			continue
		}
		if result.Size < minAlbumBytes || (maxSizeBytes > 0 && result.Size > maxSizeBytes) {
			continue
		}
		matchedAlbum, matchedArtist := tokenMatch(title, albumTokens), tokenMatch(title, artistTokens)
		if len(albumTokens) > 0 && matchedAlbum == 0 {
			continue
		}
		if len(artistTokens) > 0 && matchedArtist == 0 {
			continue
		}
		// A self-titled album has no album token to discriminate with: "karate"
		// anywhere in the title satisfies both sides. Require the pair to be spelled
		// out contiguously ("Karate - Karate"), which a soundtrack starting with the
		// same word does not do.
		if sameTokens(artistTokens, albumTokens) && !strings.Contains(title, selfTitledPhrase(artistTokens)) {
			continue
		}

		score := result.Seeders
		if score > 50 {
			score = 50
		}
		if len(albumTokens) > 0 && matchedAlbum == len(albumTokens) {
			score += 40
		}
		if matchedArtist == len(artistTokens) {
			score += 15
		}
		if strings.Contains(title, " eac ") || strings.HasSuffix(title, " eac") {
			score += 10
		}
		if strings.Contains(title, " 24 ") || strings.Contains(title, "24bit") {
			score += 5
		}

		candidates = append(candidates, Candidate{
			Title:       result.Title,
			Hash:        hash,
			DownloadURL: link,
			Seeders:     result.Seeders,
			Size:        result.Size,
			Score:       score,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return better(candidates[i], candidates[j]) })
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func better(a, b Candidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Seeders != b.Seeders {
		return a.Seeders > b.Seeders
	}
	return a.Size < b.Size
}

// tokenMatch counts how many of want appear in the normalized title.
func tokenMatch(title string, want []string) int {
	matched := 0
	for _, token := range want {
		if strings.Contains(title, " "+token+" ") || strings.HasPrefix(title, token+" ") || strings.HasSuffix(title, " "+token) {
			matched++
		}
	}
	return matched
}

// sameTokens reports whether two token sets are equal: the album is self-titled.
func sameTokens(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, token := range a {
		counts[token]++
	}
	for _, token := range b {
		counts[token]--
		if counts[token] < 0 {
			return false
		}
	}
	for _, left := range counts {
		if left != 0 {
			return false
		}
	}
	return true
}

// selfTitledPhrase is the contiguous "artist album" sequence a real self-titled
// release spells out; the title is normalized and padded, so leading and trailing
// spaces make it a whole-word match.
func selfTitledPhrase(tokens []string) string {
	joined := strings.Join(tokens, " ")
	return " " + joined + " " + joined + " "
}

// normalizeTitle lowercases and turns every non-alphanumeric run into one space,
// with the string padded so token boundaries can be tested with Contains.
func normalizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte(' ')
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	if !lastSpace {
		b.WriteByte(' ')
	}
	return b.String()
}

// normalizeTokens returns the meaningful tokens of a name: short words and the
// words that carry no identity on their own are dropped.
func normalizeTokens(s string) []string {
	stop := map[string]bool{"the": true, "and": true, "a": true, "an": true, "of": true,
		"vol": true, "cd": true, "disc": true, "deluxe": true, "edition": true, "remastered": true}
	var tokens []string
	for _, token := range strings.Fields(normalizeTitle(s)) {
		if len(token) < 3 || stop[token] {
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}
