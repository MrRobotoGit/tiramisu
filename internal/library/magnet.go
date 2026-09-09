package library

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var reMagnetHash = regexp.MustCompile(`(?i)xt=urn:btih:([a-z0-9]{32,40})`)

// BuildMagnet creates a magnet URL from an info hash and optional trackers.
func BuildMagnet(infoHash, name string, trackers []string) string {
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
	if name != "" {
		magnet += fmt.Sprintf("&dn=%s", url.QueryEscape(name))
	}
	for _, tr := range trackers {
		magnet += fmt.Sprintf("&tr=%s", url.QueryEscape(tr))
	}
	return magnet
}

// DefaultTrackers returns the fallback tracker list.
func DefaultTrackers() []string {
	return []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
	}
}

// HashFromMagnet extracts the lowercase info hash from a magnet URI, or "".
func HashFromMagnet(magnet string) string {
	m := reMagnetHash.FindStringSubmatch(magnet)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}
