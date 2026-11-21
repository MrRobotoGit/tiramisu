package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"tiramisu/internal/engine/bt"
)

var (
	ErrNoBlocklist    = errors.New("engine: no blocklist")
	ErrEmptyBlocklist = bt.ErrEmptyBlocklist
)

// Blocklist is a loaded IP blocklist.
type Blocklist struct{ b *bt.Blocklist }

func (b *Blocklist) NumRanges() int {
	if b == nil {
		return 0
	}
	return b.b.NumRanges()
}

func (b *Blocklist) bt() *bt.Blocklist {
	if b == nil {
		return nil
	}
	return b.b
}

type blocklistState struct {
	mu      sync.Mutex
	enabled bool
	filter  *regexp.Regexp
	current *Blocklist // the list the client uses, also at the next connect
	binDir  string
}

// SetBlockListEnabled is set by main before Start and on every config change; off clears the
// current list, so the next connect installs none.
func (e *Engine) SetBlockListEnabled(on bool) {
	e.block.mu.Lock()
	e.block.enabled = on
	if !on {
		e.block.current = nil
	}
	e.block.mu.Unlock()
}

// SetBlockListFilter sets the description filter; "" keeps every range. An invalid pattern
// is an error and loading continues unfiltered.
func (e *Engine) SetBlockListFilter(pattern string) error {
	var re *regexp.Regexp
	var err error
	if pattern != "" {
		if re, err = regexp.Compile(pattern); err != nil {
			re = nil
			err = fmt.Errorf("engine: blocklist filter: %w", err)
		}
	}
	e.block.mu.Lock()
	e.block.filter = re
	e.block.mu.Unlock()
	return err
}

// ReadBlockedIP loads <state>/blocklist, else <binary dir>/blocklist. Disabled or missing:
// ErrNoBlocklist; zero ranges: ErrEmptyBlocklist, never an empty list.
func (e *Engine) ReadBlockedIP() (*Blocklist, error) {
	e.block.mu.Lock()
	on, filter, binDir := e.block.enabled, e.block.filter, e.block.binDir
	e.block.mu.Unlock()
	if !on {
		return nil, ErrNoBlocklist
	}
	var f *os.File
	for _, dir := range []string{e.deps.StateDir, binDir} {
		if dir == "" {
			continue
		}
		var err error
		if f, err = os.Open(filepath.Join(dir, "blocklist")); err == nil {
			break
		}
	}
	if f == nil {
		return nil, ErrNoBlocklist
	}
	defer f.Close()
	b, err := bt.LoadBlocklist(f, filter)
	if err != nil {
		return nil, err
	}
	return &Blocklist{b: b}, nil
}

// CountRanges is the sanity gate main applies to a downloaded file before installing it.
func CountRanges(r io.Reader) int { return bt.CountRanges(r) }

// SetIPBlocklist installs b (nil: no filtering) on the running client and for later connects.
func (e *Engine) SetIPBlocklist(b *Blocklist) {
	e.block.mu.Lock()
	e.block.current = b
	e.block.mu.Unlock()
	if c := e.currentClient(); c != nil {
		c.SetBlocklist(b.bt())
	}
}

// connectBlocklist is the list for a new client: none when disabled, else the current one,
// else the file, loaded and installed (a load failure is logged and the client is unfiltered).
func (e *Engine) connectBlocklist() *bt.Blocklist {
	e.block.mu.Lock()
	on, cur := e.block.enabled, e.block.current
	e.block.mu.Unlock()
	if !on {
		return nil
	}
	if cur != nil {
		return cur.bt()
	}
	b, err := e.ReadBlockedIP()
	if err != nil {
		e.deps.logf("[Tentacle] blocklist not loaded (%v), peers unfiltered", err)
		return nil
	}
	e.block.mu.Lock()
	if e.block.current == nil && e.block.enabled {
		e.block.current = b
	}
	cur = e.block.current
	e.block.mu.Unlock()
	return cur.bt()
}

func (e *Engine) installedBlocklist() *bt.Blocklist {
	e.block.mu.Lock()
	defer e.block.mu.Unlock()
	return e.block.current.bt()
}

func binaryDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}
