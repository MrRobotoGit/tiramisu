// Package piecemap builds the dashboard's piece map: the pieces from a player's read position to
// the download frontier, and how many bytes are ready before the first gap.
package piecemap

const (
	// Cells is how many cells the strip returns: 32 visible plus two that scroll in from the right.
	// BehindCells of them sit behind the playhead.
	Cells       = 34
	BehindCells = 2
	// MaxScan bounds how far ahead of the playhead the frontier is looked for.
	MaxScan = 2048
)

// Piece is one torrent piece as the engine reports it.
type Piece struct {
	Complete bool
	Length   int64
	Missing  int64
	Deadline bool
}

// Source is what Build reads; offsets are in file coordinates except FileOffset.
type Source struct {
	Hash       string
	PieceLen   int64
	FileOffset int64 // file start inside the torrent
	FileSize   int64
	PlayerOff  int64
	SpanBytes  int64 // bytes ahead the strip covers; 0 sizes it to the download frontier
	Window     func(first, n int) []Piece
	Covered    func(off, end int64) bool // FUSE read-ahead buffer holds [off, end)
}

// Cell states: b played and still in memory, e played and evicted, r ready,
// d downloading with a stream deadline, p partly here, m not requested.
// A cell covers Bucket pieces starting at I; Pct is the share of its bytes already here.
type Cell struct {
	I   int    `json:"i"`
	S   string `json:"s"`
	Pct int    `json:"pct,omitempty"`
}

type Map struct {
	Hash       string  `json:"hash"`
	PieceLen   int64   `json:"piece_len"`
	FileSize   int64   `json:"file_size"`
	FileFirst  int     `json:"file_first"` // first piece of the file
	FilePieces int     `json:"file_pieces"`
	PlayerOff  int64   `json:"player_off"`
	Play       int     `json:"play"` // piece under the playhead
	PlayFrac   float64 `json:"play_frac"`
	AheadBytes int64   `json:"ahead_bytes"`
	FirstGap   int     `json:"first_gap"`  // -1 when nothing is missing up to the end of the scan
	DoneAhead  bool    `json:"done_ahead"` // everything from the playhead to the end of the file is here
	Bucket     int     `json:"bucket"`     // pieces per cell, a power of two
	Cells      []Cell  `json:"cells"`
}

type pstate struct {
	s    byte // r d p m
	have int64
	size int64
}

// Build returns nil when the source cannot describe a window.
func Build(s Source) *Map {
	pl := s.PieceLen
	if pl <= 0 || s.FileSize <= 0 || s.Window == nil {
		return nil
	}
	off := min(max(s.PlayerOff, 0), s.FileSize-1)
	fileFirst := int(s.FileOffset / pl)
	fileLast := int((s.FileOffset + s.FileSize - 1) / pl)
	play := int((s.FileOffset + off) / pl)
	bounds := func(i int) (int64, int64) {
		return max(int64(i)*pl-s.FileOffset, 0), min(int64(i+1)*pl-s.FileOffset, s.FileSize)
	}
	covered := func(a, b int64) bool { return s.Covered != nil && s.Covered(a, b) }

	// Ahead of the playhead: state of every piece up to the scan limit.
	scan := min(MaxScan, fileLast-play+1)
	raw := s.Window(play, scan)
	ahead := make([]pstate, len(raw))
	m := &Map{Hash: s.Hash, PieceLen: pl, FileSize: s.FileSize, FileFirst: fileFirst, FilePieces: fileLast - fileFirst + 1,
		PlayerOff: off, Play: play, PlayFrac: float64((s.FileOffset+off)%pl) / float64(pl), FirstGap: -1}
	gapK, lastBusy := -1, -1
	for k, p := range raw {
		i := play + k
		a, b := bounds(i)
		from := a
		if k == 0 {
			from = off
		}
		st := pstate{size: b - a}
		switch {
		case p.Complete || covered(from, b):
			st.s, st.have = 'r', b-a
		case p.Length > 0 && p.Missing < p.Length:
			st.s, st.have = 'p', (p.Length-p.Missing)*(b-a)/p.Length
			if p.Deadline {
				st.s = 'd'
			}
		case p.Deadline:
			st.s = 'd'
		default:
			st.s = 'm'
		}
		if st.s == 'p' || st.s == 'd' {
			lastBusy = k
		}
		if gapK < 0 {
			if st.s == 'r' {
				m.AheadBytes += b - from
			} else {
				gapK = k
			}
		}
		ahead[k] = st
	}
	// The strip ends a little past the frontier: the first gap or the last piece still arriving.
	endK := len(ahead) - 1
	if gapK >= 0 {
		m.FirstGap = play + gapK
		e := max(gapK, lastBusy)
		endK = min(len(ahead)-1, e+max(2, e/4))
	} else {
		m.DoneAhead = play+len(ahead)-1 == fileLast
	}
	// A fixed span keeps the scale still between polls: sized to the frontier, the bucket flipped
	// whenever a far piece started or finished arriving.
	want := endK + 1
	if s.SpanBytes > 0 {
		want = int((s.SpanBytes + pl - 1) / pl)
	}
	bucket := 1
	for bucket*(Cells-BehindCells) < want {
		bucket *= 2
	}
	m.Bucket = bucket
	// Cells are aligned to multiples of bucket so their bounds hold still while the playhead moves.
	start := max(fileFirst, (play/bucket-BehindCells)*bucket)
	var behind []Piece
	if start < play {
		behind = s.Window(start, play-start)
	}
	// Always fill the strip: past the frontier the extra cells show what is not requested yet.
	last := min(fileLast, start+Cells*bucket-1, play+len(ahead)-1)
	for a := start; a <= last; a += bucket {
		b := min(a+bucket, last+1)
		var have, size int64
		anyDL, allR, allM, anyAhead, kept := false, true, true, false, false
		for i := a; i < b; i++ {
			if i < play {
				if j := i - start; j < len(behind) {
					pa, pb := bounds(i)
					if behind[j].Complete || covered(pa, pb) {
						kept = true
					}
				}
				continue
			}
			anyAhead = true
			st := ahead[i-play]
			have += st.have
			size += st.size
			anyDL = anyDL || st.s == 'd'
			allR = allR && st.s == 'r'
			allM = allM && st.s == 'm'
		}
		c := Cell{I: a}
		switch {
		case !anyAhead && kept:
			c.S = "b"
		case !anyAhead:
			c.S = "e"
		case allR:
			c.S = "r"
		case allM && !anyDL:
			c.S = "m"
		default:
			c.S = "p"
			if anyDL {
				c.S = "d"
			}
			if size > 0 {
				c.Pct = int(have * 100 / size)
			}
		}
		m.Cells = append(m.Cells, c)
	}
	return m
}
