package utils

import (
	"context"
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
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.theoks.net:6969/announce",

	// Tier 3: HTTP/HTTPS fallback
	"http://tracker.opentrackr.org:1337/announce",
	"https://tracker.lilithraws.org:443/announce",

	// RuTracker's anonymous announce (the form its magnets carry). Its small lossless
	// swarms live there more than in the DHT; for a hash it does not track the
	// tracker just answers with nothing.
	"http://bt.t-ru.org/ann?magnet",
	"http://bt2.t-ru.org/ann?magnet",
	"http://bt3.t-ru.org/ann?magnet",
	"http://bt4.t-ru.org/ann?magnet",
}
var (
	loadedTrackers []string
	trackersMu     sync.Mutex
	trackersOnce   sync.Once

	onTrackersLoaded   func([]string)
	onTrackersLoadedMu sync.Mutex
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

// maxTrackersListBytes bounds what a mirror can make us allocate: the real list
// is a few tens of KB, and io.ReadAll on a hostile or broken mirror is not.
const maxTrackersListBytes = 1 << 20

// trackersClient is shared so the mirror chain reuses connections. It carries no
// Timeout: the per-mirror deadline and the shutdown cancellation both travel on
// the request context.
var trackersClient = &http.Client{}

// trackersCtx is cancelled by StopTrackerLoader, ending the refresh loop and any
// fetch in flight.
var trackersCtx, trackersCancel = context.WithCancel(context.Background())

// StopTrackerLoader stops the periodic tracker refresh and cancels a fetch in
// flight. Safe to call more than once.
func StopTrackerLoader() { trackersCancel() }

// SetOnTrackersLoaded registers a callback invoked after every successful
// tracker-list load or refresh with the merged remote+built-in list. Torrents
// that start before the first fetch returns (and long-lived ones predating a
// refresh) would otherwise keep the fallback list for their whole life:
// spec.Trackers is built once, and nothing else re-applies the list.
func SetOnTrackersLoaded(fn func([]string)) {
	onTrackersLoadedMu.Lock()
	onTrackersLoaded = fn
	onTrackersLoadedMu.Unlock()
	if fn == nil {
		return
	}
	// Catch-up: the loader starts on the first GetDefTrackers call, so a list
	// may already be loaded before the hook is registered. Applying it here
	// keeps the fan-out independent from initialization order.
	trackersMu.Lock()
	list := loadedTrackers
	trackersMu.Unlock()
	if len(list) > 0 {
		fn(append([]string(nil), list...))
	}
}

func notifyTrackersLoaded(list []string) {
	onTrackersLoadedMu.Lock()
	fn := onTrackersLoaded
	onTrackersLoadedMu.Unlock()
	if fn != nil {
		fn(list)
	}
}

func GetTrackerFromFile() []string {
	name := filepath.Join(settings.Path, "trackers.txt")
	buf, err := os.ReadFile(name)
	if err == nil {
		return parseTrackerLines(buf)
	}
	return nil
}

func GetDefTrackers() []string {
	trackersOnce.Do(func() { go retryLoadTrackers(trackersCtx) })

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
func retryLoadTrackers(ctx context.Context) {
	delay := 30 * time.Second
	attempt := 1
	everLoaded := false
	for {
		if err := loadNewTracker(ctx); err == nil {
			trackersMu.Lock()
			n := len(loadedTrackers)
			trackersMu.Unlock()
			// Distinct wording: in a week of logs a periodic refresh must not read
			// like a restart.
			if everLoaded {
				log.TLogln("Tracker list refreshed:", n, "trackers")
			} else {
				log.TLogln("Tracker list loaded:", n, "trackers")
			}
			everLoaded = true
			delay = 30 * time.Second
			attempt = 1
			if !waitOrStop(ctx, trackersRefreshInterval) {
				return
			}
			continue
		} else if ctx.Err() != nil {
			return
		} else {
			trackersMu.Lock()
			loaded := len(loadedTrackers)
			trackersMu.Unlock()
			if loaded > 0 {
				log.TLogln("Tracker list refresh failed (attempt", attempt, "):", err, "\u2014 keeping", loaded, "loaded trackers, retrying in", delay)
			} else {
				log.TLogln("Tracker list download failed (attempt", attempt, "):", err, "\u2014 using", len(defTrackers), "built-in trackers, retrying in", delay)
			}
		}
		if !waitOrStop(ctx, delay) {
			return
		}
		if delay < 30*time.Minute {
			delay *= 2
		}
		attempt++
	}
}

// waitOrStop sleeps for d, returning false when ctx is cancelled first.
func waitOrStop(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// loadNewTracker walks the mirror chain and returns on the first mirror that
// answers with a usable list. The loaded list is replaced only on success, so a
// chain where every mirror fails leaves the previous list in place.
func loadNewTracker(ctx context.Context) error {
	var errs []error
	for _, url := range trackersListURLs {
		ret, err := fetchTrackersFromURL(ctx, url)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("tracker fetch aborted: %w", ctx.Err())
			}
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			continue
		}
		merged := mergeTrackers(ret, defTrackers)
		trackersMu.Lock()
		loadedTrackers = merged
		trackersMu.Unlock()
		notifyTrackersLoaded(merged)
		return nil
	}
	return fmt.Errorf("all %d mirrors failed: %w", len(trackersListURLs), errors.Join(errs...))
}

func fetchTrackersFromURL(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, trackersFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := trackersClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// One byte past the cap tells a truncated read from a legitimate one.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxTrackersListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > maxTrackersListBytes {
		return nil, fmt.Errorf("body over %d bytes", maxTrackersListBytes)
	}
	ret := parseTrackerLines(buf)
	if len(ret) == 0 {
		return nil, fmt.Errorf("no announce URL in %d bytes", len(buf))
	}
	return ret, nil
}

// parseTrackerLines keeps only announce URLs. A mirror answering 200 with an
// error page would otherwise win the chain and feed its markup to the client as
// trackers; rejecting it here is what makes the failover cover that case.
func parseTrackerLines(buf []byte) []string {
	var ret []string
	for _, s := range strings.Split(string(buf), "\n") {
		// Trim first: a CRLF file leaves a trailing \r inside the announce URL.
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "udp://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			ret = append(ret, s)
		}
	}
	return ret
}

// mergeTrackers concatenates in order, dropping repeats: the remote list already
// carries most built-ins, and a duplicate URL means announcing twice to the same
// tracker on every torrent.
func mergeTrackers(lists ...[]string) []string {
	seen := make(map[string]struct{})
	var ret []string
	for _, list := range lists {
		for _, t := range list {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			ret = append(ret, t)
		}
	}
	return ret
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
