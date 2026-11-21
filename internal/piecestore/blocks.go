package piecestore

import "math/bits"

// BlockSize is the BitTorrent request unit.
const BlockSize = 16 << 10

// unitSize is the granularity of written coverage. A unit is readable once one write covers it
// whole; a write not aligned to it leaves its edge units unreadable until MarkComplete.
const unitSize = 1 << 10

// unitSet records which units of one piece hold written bytes.
type unitSet struct {
	words []uint64
	n     int
	count int
}

func newUnitSet(n int) unitSet {
	return unitSet{words: make([]uint64, (n+63)/64), n: n}
}

func (s *unitSet) set(i int) {
	w, b := i/64, uint(i%64)
	if s.words[w]&(1<<b) == 0 {
		s.words[w] |= 1 << b
		s.count++
	}
}

func (s *unitSet) has(i int) bool { return s.words[i/64]&(1<<uint(i%64)) != 0 }

func (s *unitSet) full() bool { return s.n > 0 && s.count == s.n }

// runFrom returns how many consecutive written units start at i.
func (s *unitSet) runFrom(i int) int {
	j := i
	for j < s.n {
		w := ^s.words[j/64] >> uint(j%64) // ones where units are missing
		if w == 0 {
			j += 64 - j%64
			continue
		}
		j += bits.TrailingZeros64(w)
		break
	}
	return min(j, s.n) - i
}

func (s *unitSet) fill() {
	for i := range s.n {
		s.set(i)
	}
}

func (s *unitSet) reset() {
	clear(s.words)
	s.count = 0
}
