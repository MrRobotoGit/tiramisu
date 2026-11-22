package streamer

import (
	"time"

	"tiramisu/internal/piecestore"
)

// evict runs the eviction scan of storage.md §3.2: throttled to 1s below 90% of the cache,
// stranded partials every 5s, then verified pieces outside every window, LRU first.
func (t *Torrent) evict(now time.Time, tun Tunables, spans []span, sh shares) {
	c := tun.CacheSize
	filled := t.ts.Filled()
	if now.Sub(t.lastScan) < evictThrottle && filled*100 < c*evictPressure {
		return
	}
	t.lastScan = now
	pl, np := t.ts.PieceLen(), t.ts.NumPieces()
	var prot, keep []piecestore.Window
	for _, sp := range spans {
		switch sp.kind {
		case KindStream:
			prot = append(prot, streamWindow(sp, sh, np))
			keep = append(keep, edgeWindows(sp, pl, np)...)
		case KindDemand, KindPrefetch:
			prot = append(prot, demandWindow(sp, pl, np))
		}
	}
	keep = append(keep, prot...)
	snap := t.ts.Snapshot()
	if now.Sub(t.lastSweep) >= sweepEvery {
		t.lastSweep = now
		t.sweep(now, snap, prot)
		filled = t.ts.Filled()
	}
	sel := piecestore.SelectEvictions(snap, keep, c, filled)
	if len(sel) == 0 {
		if filled > c && now.Sub(t.lastStarve) >= starvedLogStep {
			t.lastStarve = now
			h := cacheHolders(snap, prot, keep)
			t.s.cfg.Logf("[CacheEvict] nothing evictable: filled=%dMB capacity=%dMB protected/reader(MB)=%d readers=%d torrent=%s piece=%dMB held(MB): window=%d edges=%d written=%d partial=%d other=%d demands=%d",
				filled>>20, c>>20, sh.protected>>20, countStreams(spans), t.ts.Hash(), pl>>20,
				h.window>>20, h.edges>>20, h.written>>20, h.partial>>20, h.other>>20, len(spans)-countStreams(spans))
		}
		return
	}
	byIndex := make(map[int]piecestore.PieceInfo, len(snap))
	for _, pi := range snap {
		byIndex[pi.Index] = pi
	}
	infos := make([]piecestore.PieceInfo, 0, len(sel))
	for _, i := range sel {
		infos = append(infos, byIndex[i])
	}
	_, evicted := t.ts.Evict(infos)
	none := map[int]piecestore.Priority{}
	for _, i := range evicted {
		t.ctl.UpdateCompletion(i) // the engine must not believe a freed piece is complete
		if _, ok := t.local[i]; ok {
			none[i] = piecestore.PriorityNone
			delete(t.local, i)
		}
	}
	if len(none) > 0 {
		t.ctl.SetPiecePriorities(none)
	}
}

// sweep frees partial pieces a seek abandoned: the engine forgets them first, the store
// frees them only if no write landed since the snapshot (storage.md §3.6).
func (t *Torrent) sweep(now time.Time, snap []piecestore.PieceInfo, prot []piecestore.Window) {
	writes := make(map[int]uint64, len(snap))
	for _, pi := range snap {
		writes[pi.Index] = pi.Writes
	}
	for _, i := range piecestore.StrandedPartials(snap, prot, now.Unix(), strandedIdle) {
		if t.ctl.ForgetPartialPiece(i) {
			t.ts.Piece(i).ReleaseIfUnwritten(writes[i])
		}
	}
}

// holders splits the cache by what keeps it resident: verified pieces in a protected window or in
// a head/tail zone, written pieces awaiting the hash, partial pieces, anything else.
type holders struct{ window, edges, written, partial, other int64 }

func cacheHolders(snap []piecestore.PieceInfo, prot, keep []piecestore.Window) (h holders) {
	in := func(ws []piecestore.Window, i int) bool {
		for _, w := range ws {
			if i >= w.First && i <= w.Last {
				return true
			}
		}
		return false
	}
	for _, p := range snap {
		switch {
		case p.State == piecestore.Written:
			h.written += p.Allocated
		case p.State == piecestore.Partial:
			h.partial += p.Allocated
		case in(prot, p.Index):
			h.window += p.Allocated
		case in(keep, p.Index):
			h.edges += p.Allocated
		default:
			h.other += p.Allocated
		}
	}
	return h
}

func countStreams(spans []span) int {
	n := 0
	for _, sp := range spans {
		if sp.kind == KindStream {
			n++
		}
	}
	return n
}
