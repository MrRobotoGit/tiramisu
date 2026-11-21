package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TrackerMirrors serve ngosang's trackers_best_ip.txt, tried in order.
var TrackerMirrors = []string{
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt",
	"https://ngosang.github.io/trackerslist/trackers_best_ip.txt",
	"https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best_ip.txt",
	"https://raw.githack.com/ngosang/trackerslist/master/trackers_best_ip.txt",
}

var errNoAnnounce = errors.New("no announce URL in the answer")

// trackerLoader keeps the remote tracker list merged with the built-ins (engine.md §1.7).
type trackerLoader struct {
	mirrors   []string
	client    *http.Client
	perMirror time.Duration
	maxBody   int64
	retryMin  time.Duration
	retryMax  time.Duration
	refresh   time.Duration
	logf      func(string, ...any)

	mu     sync.Mutex
	list   []string // nil until the first load
	onLoad func([]string)

	startOnce, stopOnce sync.Once
	cancel              context.CancelFunc
	done                chan struct{}
}

func newTrackerLoader(logf func(string, ...any)) *trackerLoader {
	return &trackerLoader{
		mirrors: TrackerMirrors, client: &http.Client{}, perMirror: 5 * time.Second, maxBody: 1 << 20,
		retryMin: 30 * time.Second, retryMax: 30 * time.Minute, refresh: 12 * time.Hour, logf: logf,
	}
}

// List is the loaded list, or the built-ins while nothing has loaded: never empty.
func (l *trackerLoader) List() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.list != nil {
		return append([]string(nil), l.list...)
	}
	return append([]string(nil), BuiltinTrackers...)
}

// SetOnLoad registers the fan-out; a list already loaded is applied at once (catch-up).
func (l *trackerLoader) SetOnLoad(f func([]string)) {
	l.mu.Lock()
	l.onLoad = f
	cur := l.list
	l.mu.Unlock()
	if f != nil && cur != nil {
		f(append([]string(nil), cur...))
	}
}

func (l *trackerLoader) Start() {
	l.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		l.mu.Lock()
		l.cancel, l.done = cancel, make(chan struct{})
		l.mu.Unlock()
		go l.run(ctx)
	})
}

// Stop cancels the loop and any fetch in flight; idempotent, safe before Start.
func (l *trackerLoader) Stop() {
	l.stopOnce.Do(func() {
		l.startOnce.Do(func() {}) // a later Start does nothing
		l.mu.Lock()
		cancel, done := l.cancel, l.done
		l.mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
	})
}

func (l *trackerLoader) run(ctx context.Context) {
	defer close(l.done)
	backoff, loaded := l.retryMin, false
	for {
		var wait time.Duration
		remote, err := l.fetchAll(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			l.logf("[Trackers] remote list unavailable (%v), retry in %s", err, backoff)
			wait, backoff = backoff, min(backoff*2, l.retryMax)
		default:
			merged := mergeTrackerLists(remote, BuiltinTrackers)
			l.mu.Lock()
			l.list = merged
			hook := l.onLoad
			l.mu.Unlock()
			if loaded {
				l.logf("[Trackers] remote list refreshed: %d trackers", len(merged))
			} else {
				l.logf("[Trackers] remote list loaded: %d trackers", len(merged))
			}
			if hook != nil {
				hook(append([]string(nil), merged...))
			}
			loaded, backoff, wait = true, l.retryMin, l.refresh
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// fetchAll tries each mirror with its own timeout; the first with an announce URL wins.
func (l *trackerLoader) fetchAll(ctx context.Context) ([]string, error) {
	var errs []error
	for _, m := range l.mirrors {
		list, err := l.fetch(ctx, m)
		if err == nil {
			return list, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", m, err))
	}
	if len(errs) == 0 {
		return nil, errors.New("no mirrors")
	}
	return nil, errors.Join(errs...)
}

func (l *trackerLoader) fetch(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.perMirror)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, l.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > l.maxBody {
		return nil, fmt.Errorf("body over %d bytes", l.maxBody)
	}
	list := parseTrackerList(body)
	if len(list) == 0 {
		return nil, errNoAnnounce
	}
	return list, nil
}

// parseTrackerList keeps trimmed udp/http/https lines (CRLF safe); markup yields nothing.
func parseTrackerList(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "udp://") || strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			out = append(out, line)
		}
	}
	return out
}

func mergeTrackerLists(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, u := range l {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// fanOutTrackers adds a freshly loaded list as one tier to every hydrated torrent: spec
// trackers are built at add time only (5bbf950). Only modes 1 and 3 use the default list.
func (e *Engine) fanOutTrackers(list []string) {
	if m := e.config().RetrackersMode; m != 1 && m != 3 {
		return
	}
	for _, t := range e.ListHydrated() {
		t.bt.AddTrackers([][]string{list})
		t.mu.Lock()
		t.tiers = mergeStoredTiers(t.tiers, [][]string{list})
		t.mu.Unlock()
	}
}
