package piecestore

import (
	"slices"
	"sort"
)

// Window is an inclusive range of piece indexes that must stay resident.
type Window struct{ First, Last int }

// mergeWindows sorts windows and fuses the overlapping or touching ones.
func mergeWindows(ws []Window) []Window {
	if len(ws) == 0 {
		return nil
	}
	s := slices.Clone(ws)
	slices.SortFunc(s, func(a, b Window) int { return a.First - b.First })
	out := []Window{s[0]}
	for _, w := range s[1:] {
		last := &out[len(out)-1]
		if w.First <= last.Last+1 {
			last.Last = max(last.Last, w.Last)
			continue
		}
		out = append(out, w)
	}
	return out
}

// inWindows reports whether piece i is inside merged windows ws.
func inWindows(ws []Window, i int) bool {
	k := sort.Search(len(ws), func(k int) bool { return ws[k].Last >= i })
	return k < len(ws) && ws[k].First <= i
}

// SelectEvictions picks verified pieces outside every window, least recently accessed first,
// ties by index, until they hold more than filled-capacity bytes. Incomplete pieces are never
// chosen: the engine still counts their chunks as its own.
func SelectEvictions(pieces []PieceInfo, windows []Window, capacity, filled int64) []int {
	if filled <= capacity {
		return nil
	}
	ws := mergeWindows(windows)
	var cands []PieceInfo
	for _, p := range pieces {
		if p.Allocated > 0 && p.State == Verified && !inWindows(ws, p.Index) {
			cands = append(cands, p)
		}
	}
	slices.SortStableFunc(cands, func(a, b PieceInfo) int {
		if a.Accessed != b.Accessed {
			if a.Accessed < b.Accessed {
				return -1
			}
			return 1
		}
		return a.Index - b.Index
	})
	var out []int
	for sum := int64(0); len(out) < len(cands) && sum <= filled-capacity; {
		sum += cands[len(out)].Allocated
		out = append(out, cands[len(out)].Index)
	}
	return out
}

// StrandedPartials lists incomplete pieces outside every window, untouched for idle seconds.
// The caller frees one only after the engine accepted to forget it and with the write count
// sampled before asking (storage.md §3.6).
func StrandedPartials(pieces []PieceInfo, windows []Window, now, idle int64) []int {
	ws := mergeWindows(windows)
	var out []int
	for _, p := range pieces {
		if p.Allocated > 0 && (p.State == Partial || p.State == Written) &&
			!inWindows(ws, p.Index) && now-p.Accessed >= idle {
			out = append(out, p.Index)
		}
	}
	return out
}
