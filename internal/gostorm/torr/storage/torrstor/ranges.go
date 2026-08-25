package torrstor

import (
	"sort"

	"github.com/anacrolix/torrent"
)

type Range struct {
	Start, End int
	File       *torrent.File
}

func inRanges(ranges []Range, ind int) bool {
	i := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End >= ind
	})
	return i < len(ranges) && ranges[i].Start <= ind
}

// fillPieceInRange resets dst and marks the pieces covered by the readers'
// windows. Range.Start/End are absolute piece indices: marking the whole file
// instead would protect every piece and stop eviction entirely.
func fillPieceInRange(dst []bool, ranges []Range, pieceCount int) {
	for i := range dst {
		dst[i] = false
	}
	for _, rng := range ranges {
		start, end := rng.Start, rng.End
		if start < 0 {
			start = 0
		}
		if end >= pieceCount {
			end = pieceCount - 1
		}
		for i := start; i <= end; i++ {
			dst[i] = true
		}
	}
}

// pieceEvictable reports whether a cached piece may be dropped. A partially
// written piece must stay: anacrolix keeps its own chunk bookkeeping, so
// dropping one fails the hash check and bans the peer that supplied it.
func pieceEvictable(size int64, complete, inReaderWindow bool) bool {
	return size > 0 && complete && !inReaderWindow
}

func mergeRange(ranges []Range) []Range {
	if len(ranges) <= 1 {
		return ranges
	}
	// copy ranges
	merged := append([]Range(nil), ranges...)

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Start < merged[j].Start {
			return true
		}
		if merged[i].Start == merged[j].Start && merged[i].End < merged[j].End {
			return true
		}
		return false
	})

	j := 0
	for i := 1; i < len(merged); i++ {
		if merged[j].End >= merged[i].Start {
			if merged[j].End < merged[i].End {
				merged[j].End = merged[i].End
			}
		} else {
			j++
			merged[j] = merged[i]
		}
	}
	return merged[:j+1]
}

// prefetchBudgetPct is the share of the cache the in-flight prefetch front may pin. At the
// production 128MB/4MB configuration it yields the count of 12 that ran clean, so piece sizes
// that already fit keep their behaviour and only oversized ones are trimmed.
const prefetchBudgetPct = 40

// pieceBudgetCount caps a prefetch count by the bytes it pins rather than the pieces it names.
// An incomplete piece cannot be evicted - anacrolix keeps its own chunk bookkeeping, so dropping
// one fails the hash check and bans the peer that supplied it - and piece length varies 1MB..8MB
// across a library, so a fixed count reserves 8x more cache on one torrent than on another. Only
// ever lowers count: raising it would widen the request front on small-piece torrents and trade
// sequential focus for breadth.
func pieceBudgetCount(count int, capacity, pieceLength int64) int {
	if count <= 1 || capacity <= 0 || pieceLength <= 0 {
		return count
	}
	max := int(capacity * prefetchBudgetPct / 100 / pieceLength)
	if max < 1 {
		max = 1
	}
	if max < count {
		return max
	}
	return count
}
