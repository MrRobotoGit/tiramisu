package streamer

import "tiramisu/internal/piecestore"

const (
	minReadahead   = 8 << 20 // a scan burst must not starve the reader that plays
	protectPct     = 85      // 15% of the cache always stays evictable (V243)
	edgeZone       = 8 << 20 // head/tail of a file kept resident (container index, Cues)
	byteCapPct     = 40      // incomplete pieces requested at once, as a share of the cache
	aggressivePct  = 80
	deadlineMax    = 64
	deadlineMinBps = 64 << 10
)

// shares are the per-stream budgets of one pass; n is the single divisor (active streams).
type shares struct {
	capacity  int64
	pieceLen  int64
	readahead int64 // forward window, never above protected
	protected int64 // protected span per stream
	count     int   // incomplete pieces a stream may request
}

func computeShares(tun Tunables, pieceLen int64, n int, aggressive bool, aggrLimit int, filled int64) shares {
	n = max(n, 1)
	c := tun.CacheSize
	total := max(int64(minReadahead), c*int64(tun.ReadAheadPct)/100)
	s := shares{capacity: c, pieceLen: pieceLen, protected: c / int64(n) * protectPct / 100}
	s.readahead = min(max(int64(minReadahead), total/int64(n)), total, s.protected)
	limit := tun.ConnectionsLimit
	if aggrLimit > 0 {
		limit = aggrLimit
	}
	switch {
	case aggressive && filled > c:
		s.count = 1
	case aggressive:
		s.count = limit * aggressivePct / 100
	default:
		s.count = limit / n
	}
	if c > 0 && pieceLen > 0 {
		s.count = min(s.count, int(max(c*byteCapPct/100/pieceLen, 1)))
	}
	s.count = max(s.count, 1)
	return s
}

// span is a consumer as the planner sees it: absolute torrent offsets.
type span struct {
	kind      Kind
	fileStart int64 // first byte of the file
	fileEnd   int64 // one past the file's last byte
	pos       int64 // next byte to serve
	end       int64 // one past the last byte wanted
	seq       int64 // consumer open order
	waiting   int64 // when its reader began waiting on the piece of pos; 0: it has not
}

func pieceOf(off, pieceLen int64) int { return int(off / pieceLen) }

// lastPiece is the piece holding byte end-1, never past the file or the torrent.
func lastPiece(end, fileEnd, pieceLen int64, numPieces int) int {
	return min(pieceOf(min(end, fileEnd)-1, pieceLen), numPieces-1)
}

// streamWindow is the protected range of a stream: readahead forward, the rest behind.
func streamWindow(sp span, sh shares, numPieces int) piecestore.Window {
	lo := max(sp.pos-(sh.protected-sh.readahead), sp.fileStart)
	hi := min(sp.pos+sh.readahead, sp.fileEnd)
	return piecestore.Window{First: pieceOf(lo, sh.pieceLen), Last: lastPiece(max(hi, lo+1), sp.fileEnd, sh.pieceLen, numPieces)}
}

// edgeWindows are the head and tail zones of a file: max(PL, 8MB) each.
func edgeWindows(sp span, pieceLen int64, numPieces int) []piecestore.Window {
	z := max(pieceLen, edgeZone)
	head := piecestore.Window{First: pieceOf(sp.fileStart, pieceLen), Last: lastPiece(min(sp.fileStart+z, sp.fileEnd), sp.fileEnd, pieceLen, numPieces)}
	tail := piecestore.Window{First: pieceOf(max(sp.fileEnd-z, sp.fileStart), pieceLen), Last: lastPiece(sp.fileEnd, sp.fileEnd, pieceLen, numPieces)}
	return []piecestore.Window{head, tail}
}

// demandWindow is the outstanding range of a demand consumer.
func demandWindow(sp span, pieceLen int64, numPieces int) piecestore.Window {
	pos := min(sp.pos, max(sp.end-1, sp.fileStart))
	return piecestore.Window{First: pieceOf(pos, pieceLen), Last: lastPiece(max(sp.end, pos+1), sp.fileEnd, pieceLen, numPieces)}
}

// levelAt ranks piece i of a window that starts at first.
func levelAt(i, first int) piecestore.Priority {
	switch i - first {
	case 0:
		return piecestore.PriorityNow
	case 1:
		return piecestore.PriorityNext
	default:
		return piecestore.PriorityReadahead
	}
}

// desired computes the wanted level of every piece: streams request every incomplete piece of
// their readahead, as the engine reader did, the first count of them boosted; demands raise their
// whole range.
func desired(spans []span, sh shares, numPieces int, complete func(int) bool) map[int]piecestore.Priority {
	out := map[int]piecestore.Priority{}
	raise := func(i int, p piecestore.Priority) {
		if p > out[i] {
			out[i] = p
		}
	}
	for _, sp := range spans {
		if sp.pos >= sp.end {
			continue
		}
		first := pieceOf(sp.pos, sh.pieceLen)
		switch sp.kind {
		case KindStream:
			last := lastPiece(min(sp.pos+sh.readahead, sp.end), sp.fileEnd, sh.pieceLen, numPieces)
			for i, used := first, 0; i <= last; i++ {
				if complete(i) {
					continue
				}
				if used < sh.count {
					raise(i, levelAt(i, first))
				} else {
					raise(i, piecestore.PriorityReadahead)
				}
				used++
			}
		case KindDemand, KindPrefetch:
			w := demandWindow(sp, sh.pieceLen, numPieces)
			for i, k := w.First, 0; i <= w.Last; i++ {
				if complete(i) {
					continue
				}
				if sp.kind == KindPrefetch {
					raise(i, piecestore.PriorityReadahead)
				} else {
					raise(i, levelAt(k, 0)) // Now and Next go to the first incomplete pieces
				}
				k++
			}
		}
	}
	return out
}

// diff returns the engine calls that move cur to want and updates cur in place.
func diff(cur, want map[int]piecestore.Priority) map[int]piecestore.Priority {
	out := map[int]piecestore.Priority{}
	for i, p := range want {
		if cur[i] != p {
			out[i] = p
			cur[i] = p
		}
	}
	for i := range cur {
		if _, ok := want[i]; !ok {
			out[i] = piecestore.PriorityNone
			delete(cur, i)
		}
	}
	return out
}
