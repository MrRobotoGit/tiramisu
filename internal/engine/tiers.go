package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// BuiltinTrackers is the default list while no remote list has loaded (engine.md §1.7).
var BuiltinTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.theoks.net:6969/announce",
	"http://tracker.opentrackr.org:1337/announce",
	"https://tracker.lilithraws.org:443/announce",
	"http://bt.t-ru.org/ann?magnet",
	"http://bt2.t-ru.org/ann?magnet",
	"http://bt3.t-ru.org/ann?magnet",
	"http://bt4.t-ru.org/ann?magnet",
}

// applyRetrackers applies RetrackersMode to the spec tiers, then appends the extra tier.
func applyRetrackers(mode int, tiers [][]string, defaults, extra []string) [][]string {
	var out [][]string
	switch mode {
	case 0:
		out = tiers
	case 2:
	case 3:
		out = [][]string{defaults}
	default: // 1
		out = append(append([][]string(nil), tiers...), defaults)
	}
	if len(extra) > 0 {
		out = append(out, extra)
	}
	var clean [][]string
	for _, t := range out {
		if len(t) > 0 {
			clean = append(clean, append([]string(nil), t...))
		}
	}
	return clean
}

// mergeStoredTiers adds, as one tier, the stored URLs the spec does not already have.
func mergeStoredTiers(spec, stored [][]string) [][]string {
	seen := map[string]bool{}
	for _, t := range spec {
		for _, u := range t {
			seen[u] = true
		}
	}
	var extra []string
	for _, t := range stored {
		for _, u := range t {
			if u != "" && !seen[u] {
				seen[u] = true
				extra = append(extra, u)
			}
		}
	}
	if len(extra) == 0 {
		return spec
	}
	return append(spec, extra)
}

// readTrackersFile reads <state>/trackers.txt: one URL per line, CRLF safe.
func readTrackersFile(stateDir string) []string {
	if stateDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "trackers.txt"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
