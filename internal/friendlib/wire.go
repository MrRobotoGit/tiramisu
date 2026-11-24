package friendlib

import (
	"context"
	"path/filepath"
	"sync"

	"tiramisu/internal/friends"
	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
)

// Config wires the shared library of one node.
type Config struct {
	DB       *metadb.DB
	Friends  *friends.Manager
	Library  *library.Manager
	Root     string // the media root holding movies/ and tv/
	Registry library.EpisodeRegistry
	// RegisterInfo and InfoBytes are the engine's: friendlib does not import it.
	RegisterInfo func(hash string, info []byte, title string, trackers [][]string) error
	InfoBytes    func(hash string) []byte
	Hooks        *friends.LibraryHooks // nil: built from StopHeld
	// StopHeld stops the syncs a master replaces when the node gets one; WaitHeld waits until they
	// have (the scheduler's).
	StopHeld func()
	WaitHeld func(ctx context.Context) error
	Logf     func(format string, args ...any)
}

// masterHooks stops the held syncs when the node gets a master.
func masterHooks(stopHeld func(), waitHeld func(context.Context) error) *friends.LibraryHooks {
	return &friends.LibraryHooks{
		OnMaster: func(master string) {
			if master != "" && stopHeld != nil {
				stopHeld()
			}
		},
		BeforePull: waitHeld,
	}
}

// Lib is a node's wired shared library.
type Lib struct {
	Feed *Feed
	Sink *Sink
}

var (
	feedsMu  sync.Mutex
	feeds    = map[*Feed]bool{}
	observer sync.Once
)

// dispatch hands a stub write or removal to every live feed; each keeps the paths under its root.
func dispatch(path string) {
	feedsMu.Lock()
	fs := make([]*Feed, 0, len(feeds))
	for f := range feeds {
		fs = append(fs, f)
	}
	feedsMu.Unlock()
	for _, f := range fs {
		f.Touch(path)
	}
}

// Wire builds the feed and the sink and enables the library protocol; nil without friends or DB.
func Wire(cfg Config) *Lib {
	if cfg.Friends == nil || cfg.DB == nil || cfg.Library == nil {
		return nil
	}
	hasInfo := func(hash string) bool {
		ok, err := cfg.DB.HasGoStormInfo(hash)
		return err == nil && ok
	}
	l := &Lib{
		Feed: NewFeed(FeedConfig{Store: cfg.DB, Root: cfg.Root, Registry: cfg.Registry, HasInfo: hasInfo, Info: cfg.InfoBytes,
			Changed: cfg.Friends.LibraryChanged, Logf: cfg.Logf}),
		Sink: NewSink(SinkConfig{Store: cfg.DB, Library: cfg.Library, Register: cfg.RegisterInfo, HasInfo: hasInfo, Logf: cfg.Logf}),
	}
	feedsMu.Lock()
	feeds[l.Feed] = true
	feedsMu.Unlock()
	observer.Do(func() { library.SetStubObserver(dispatch) })
	go func() { // a crash between write and rename leaves a temp file next to the stub
		if n := library.RemoveStaleStubTemps(filepath.Join(cfg.Root, "movies"), filepath.Join(cfg.Root, "tv")); n > 0 && cfg.Logf != nil {
			cfg.Logf("[Friends] removed %d stale stub temp files", n)
		}
	}()
	hooks := cfg.Hooks
	if hooks == nil {
		hooks = masterHooks(cfg.StopHeld, cfg.WaitHeld)
	}
	cfg.Friends.SetLibrary(l.Feed, l.Sink, hooks)
	return l
}

// Shutdown stops every feed's walk in progress: the process is exiting.
func Shutdown() {
	feedsMu.Lock()
	defer feedsMu.Unlock()
	for f := range feeds {
		f.Close()
	}
}

// Close stops the feed from following stub writes (tests build several nodes in one process).
func (l *Lib) Close() {
	if l == nil {
		return
	}
	feedsMu.Lock()
	delete(feeds, l.Feed)
	feedsMu.Unlock()
	l.Feed.Close()
}

// AfterSync repairs every feed after a video job: a stub write that missed the funnels is found.
func AfterSync(job string) {
	if job == "music" {
		return // music is not part of the shared library
	}
	feedsMu.Lock()
	fs := make([]*Feed, 0, len(feeds))
	for f := range feeds {
		fs = append(fs, f)
	}
	feedsMu.Unlock()
	for _, f := range fs {
		f.Reconcile() // a feed without slaves returns at once
	}
}
