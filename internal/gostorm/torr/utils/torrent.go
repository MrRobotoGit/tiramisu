package utils

import (
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tiramisu/internal/gostorm/log"
	"tiramisu/internal/gostorm/settings"

	"golang.org/x/time/rate"
)

var defTrackers = []string{
	// Tier 1: Più affidabili e veloci (UDP)
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"udp://open.demonii.com:1337/announce",

	// Tier 2: Affidabili globali
	"udp://tracker.tiny-vps.com:6969/announce",
	"udp://tracker.moeking.me:6969/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://opentracker.i2p.rocks:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.theoks.net:6969/announce",

	// Tier 3: HTTP/HTTPS fallback
	"http://tracker.opentrackr.org:1337/announce",
	"https://tracker.tamersunion.org:443/announce",
	"https://tracker.lilithraws.org:443/announce",
}
var (
	loadedTrackers []string
	trackersMu     sync.Mutex
	trackersOnce   sync.Once
)

// trackersListURLs is the built-in mirror chain for the remote tracker list,
// tried in order until one answers. A single URL (raw.githubusercontent.com)
// is blocked or rate-limited often enough that one failed fetch used to leave
// the run on the built-in trackers alone; mirrors on different hosts survive
// exactly that.
var trackersListURLs = []string{
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt",
	"https://ngosang.github.io/trackerslist/trackers_best_ip.txt",
	"https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best_ip.txt",
	"https://raw.githack.com/ngosang/trackerslist/master/trackers_best_ip.txt",
}

// trackersFetchTimeout bounds each mirror attempt, not the whole chain: with a
// per-mirror timeout the failover to the next host costs seconds, while one
// shared timeout would make a hung first mirror eat the retry window.
var trackersFetchTimeout = 5 * time.Second

// trackersRefreshInterval is how often the remote list is reloaded once the
// first fetch has succeeded. Without it the list stayed frozen at whatever was
// served at boot: on a process that runs for weeks, entries that disappear from
// the upstream list keep being offered, and new ones are never picked up.
var trackersRefreshInterval = 12 * time.Hour

func GetTrackerFromFile() []string {
	name := filepath.Join(settings.Path, "trackers.txt")
	buf, err := os.ReadFile(name)
	if err == nil {
		list := strings.Split(string(buf), "\n")
		var ret []string
		for _, l := range list {
			// Trim first: a file saved with CRLF leaves a trailing \r inside the announce
			// URL, and leading spaces drop otherwise valid trackers.
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "udp") || strings.HasPrefix(l, "http") {
				ret = append(ret, l)
			}
		}
		return ret
	}
	return nil
}

func GetDefTrackers() []string {
	trackersOnce.Do(func() { go retryLoadTrackers(nil) })

	trackersMu.Lock()
	defer trackersMu.Unlock()
	if len(loadedTrackers) == 0 {
		return defTrackers
	}
	return loadedTrackers
}

// retryLoadTrackers keeps trying until the list is in. A single failed fetch at
// startup used to leave the client on the built-in trackers for the whole run,
// silently: those cover far fewer swarms, so torrents look peerless and time out.
// After the first success it keeps the list fresh, reloading every
// trackersRefreshInterval; a failed refresh keeps the previous list, and the
// exponential backoff doubles again until one succeeds.
func retryLoadTrackers(stop <-chan struct{}) {
	delay := 30 * time.Second
	attempt := 1
	for {
		if err := loadNewTracker(); err == nil {
			trackersMu.Lock()
			n := len(loadedTrackers)
			trackersMu.Unlock()
			log.TLogln("Tracker list loaded:", n, "trackers")
			delay = 30 * time.Second
			attempt = 1
			if !waitOrStop(trackersRefreshInterval, stop) {
				return
			}
			continue
		} else {
			trackersMu.Lock()
			loaded := len(loadedTrackers)
			trackersMu.Unlock()
			if loaded > 0 {
				log.TLogln("Tracker list refresh failed (attempt", attempt, "):", err, "— keeping", loaded, "loaded trackers, retrying in", delay)
			} else {
				log.TLogln("Tracker list download failed (attempt", attempt, "):", err, "— using", len(defTrackers), "built-in trackers, retrying in", delay)
			}
		}
		if !waitOrStop(delay, stop) {
			return
		}
		if delay < 30*time.Minute {
			delay *= 2
		}
		attempt++
	}
}

// waitOrStop sleeps for d, returning false when stop fires first. A nil stop
// never fires, which is how the production loop runs for the process lifetime.
func waitOrStop(d time.Duration, stop <-chan struct{}) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

// loadNewTracker walks the mirror chain and returns on the first mirror that
// answers with a usable list. The loaded list is replaced only on success, so a
// chain where every mirror fails leaves the previous list in place.
func loadNewTracker() error {
	var errs []error
	for _, url := range trackersListURLs {
		ret, err := fetchTrackersFromURL(url)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		trackersMu.Lock()
		loadedTrackers = append(ret, defTrackers...)
		trackersMu.Unlock()
		return nil
	}
	return fmt.Errorf("all %d mirrors failed: %w", len(trackersListURLs), errors.Join(errs...))
}

func fetchTrackersFromURL(url string) ([]string, error) {
	client := &http.Client{Timeout: trackersFetchTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ret []string
	for _, s := range strings.Split(string(buf), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			ret = append(ret, s)
		}
	}
	if len(ret) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return ret, nil
}

func PeerIDRandom(peer string) string {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	if err != nil {
		panic(err)
	}
	return peer + base32.StdEncoding.EncodeToString(randomBytes)[:20-len(peer)]
}

func Limit(i int) *rate.Limiter {
	l := rate.NewLimiter(rate.Inf, 0)
	if i > 0 {
		b := i
		if b < 16*1024 {
			b = 16 * 1024
		}
		l = rate.NewLimiter(rate.Limit(i), b)
	}
	return l
}
