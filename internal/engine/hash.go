package engine

import (
	"errors"
	"regexp"
	"strings"
)

var ErrMalformedHash = errors.New("engine: malformed infohash")

// ParseHash accepts 40 hex characters in any case and returns them lowercase.
func ParseHash(s string) (string, error) {
	if len(s) != 40 {
		return "", ErrMalformedHash
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return "", ErrMalformedHash
		}
	}
	return strings.ToLower(s), nil
}

// Tracker credentials in a query, raw or URL-encoded; the value stops at &, ; or %26.
var passkeyRe = regexp.MustCompile(`(?i)((?:^|[?&;]|%3F|%26)(?:pk|passkey|authkey|torrent_pass|uk|apikey)(?:=|%3D))(?:[^&;\s%]|%(?:[013-9a-f][0-9a-f]|2[0-57-9a-f]))+`)

// Redact hides tracker passkeys in a link or URL before it reaches a log.
func Redact(s string) string { return passkeyRe.ReplaceAllString(s, "${1}REDACTED") }
