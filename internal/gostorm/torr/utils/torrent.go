package utils

import (
	"encoding/base32"
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

const trackersListURL = "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt"

func GetTrackerFromFile() []string {
	name := filepath.Join(settings.Path, "trackers.txt")
	buf, err := os.ReadFile(name)
	if err == nil {
		list := strings.Split(string(buf), "\n")
		var ret []string
		for _, l := range list {
			if strings.HasPrefix(l, "udp") || strings.HasPrefix(l, "http") {
				ret = append(ret, l)
			}
		}
		return ret
	}
	return nil
}

func GetDefTrackers() []string {
	trackersOnce.Do(func() { go retryLoadTrackers() })

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
func retryLoadTrackers() {
	delay := 30 * time.Second
	for attempt := 1; ; attempt++ {
		if err := loadNewTracker(); err == nil {
			trackersMu.Lock()
			n := len(loadedTrackers)
			trackersMu.Unlock()
			log.TLogln("Tracker list loaded:", n, "trackers")
			return
		} else {
			log.TLogln("Tracker list download failed (attempt", attempt, "):", err, "— using", len(defTrackers), "built-in trackers, retrying in", delay)
		}
		time.Sleep(delay)
		if delay < 30*time.Minute {
			delay *= 2
		}
	}
}

func loadNewTracker() error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(trackersListURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var ret []string
	for _, s := range strings.Split(string(buf), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			ret = append(ret, s)
		}
	}
	if len(ret) == 0 {
		return fmt.Errorf("empty list")
	}
	trackersMu.Lock()
	loadedTrackers = append(ret, defTrackers...)
	trackersMu.Unlock()
	return nil
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
