package stallnotice

import (
	"time"

	"tiramisu/internal/notice"
	"tiramisu/internal/piecedrop"
	"tiramisu/internal/warmup"
)

// Options is what the app wires in: the rest comes from the SSD warmup and the clip bank.
type Options struct {
	Enabled    func() bool
	Goodput    func(hash string, since int64) (p50 float64, samples int, ok bool)
	GoodputSeq func(hash string) int64
	Peers      func(hash string) int
	// UnchokeSettle: see Deps.UnchokeSettle.
	UnchokeSettle func(hash string) time.Duration
	Announce      func(hash string, done func(peers, answered int))
	// CopyAny copies the bytes at off of path from the read-ahead chunk holding them
	// (racache.Cache.CopyAny): how many contiguous bytes from off.
	CopyAny  func(path string, off int64, dest []byte) int
	StopFilm func(path, hash string)
	Failed   func(path string) // the session counts as a failed playback (dead-swarm bookkeeping)
	Tick     time.Duration
	Logf     func(format string, args ...any)
	// OnCorrupt installs the engine's hash-failure hook (engine.SetOnCorrupt): the copies of a
	// failed piece are dropped from DropCache (racache.Cache.Drop) and the SSD warmup, and the
	// sessions forget them. Settle is the read path's fetch bound (piecedrop.Deps).
	OnCorrupt func(func(hash string, fileID int, off, end int64))
	DropCache func(path string, off, end int64)
	Settle    time.Duration
}

// Boot builds the manager over the SSD warmup and the read-ahead cache. Without a usable clip
// bank the feature stays off.
func Boot(o Options) *Manager {
	m := New(Deps{
		Enabled:       o.Enabled,
		Goodput:       o.Goodput,
		GoodputSeq:    o.GoodputSeq,
		Peers:         o.Peers,
		UnchokeSettle: o.UnchokeSettle,
		Announce:      o.Announce,
		Local:         localReader(o.CopyAny),
		Tail:          warmTail,
		TailSize:      warmup.TailWarmupSize,
		StopFilm:      o.StopFilm,
		Failed:        o.Failed,
		Logf:          o.Logf,
		Tick:          o.Tick,
	})
	if o.OnCorrupt != nil && o.DropCache != nil {
		o.OnCorrupt(piecedrop.Handler(piecedrop.Deps{Files: m.Files, DropCache: o.DropCache, Rollback: m.Rollback,
			DropWarmup: func(hash string, fileID int, off, end, size int64) {
				warmup.DiskWarmup.DropRange(hash, fileID, off, end, size) // nil-safe
			}, Logf: o.Logf, Settle: o.Settle}))
	}
	err := notice.BankOK()
	m.SetReady(err == nil)
	if err != nil && o.Logf != nil {
		o.Logf("[Notice] clip bank unusable, the stall notice is off: %v", err)
	}
	return m
}

func warmTail(hash string, fileID int, size int64, p []byte, off int64) int {
	if warmup.DiskWarmup == nil {
		return 0
	}
	n, _ := warmup.DiskWarmup.ReadTail(hash, fileID, p, off, size)
	return n
}

// localReader serves real bytes from the SSD head, then the read-ahead cache, never waiting.
func localReader(copyAny func(string, int64, []byte) int) func(string, string, int, int64, []byte) int {
	return func(path, hash string, fileID int, off int64, p []byte) int {
		n := 0
		if warmup.DiskWarmup != nil && off < warmup.DiskWarmup.GetAvailableRange(hash, fileID) {
			n, _ = warmup.DiskWarmup.ReadCovered(hash, fileID, p, off)
		}
		for n < len(p) && copyAny != nil { // one read-ahead chunk at a time
			k := copyAny(path, off+int64(n), p[n:])
			if k <= 0 {
				break
			}
			n += k
		}
		return n
	}
}
