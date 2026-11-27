package notice

import "bytes"

// piece is a run of bytes: data, then zeros (never allocated).
type piece struct {
	data  []byte
	zeros int64
}

func piecesLen(ps []piece) int64 {
	var n int64
	for _, p := range ps {
		n += int64(len(p.data)) + p.zeros
	}
	return n
}

// Splice is the content of [V, End): the rest of the Cluster the player is inside.
type Splice struct {
	V, End       int64
	ClusterStart int64  // the Cluster that V cuts (the prefix may span earlier ones)
	HasTime      bool   // its Timestamp was served; if not, NextTime is relative to it
	NextTime     uint64 // TimestampScale units: first free time after the last complete video block
	pieces       []piece
}

// Bytes materialises the splice (tests only).
func (s *Splice) Bytes() []byte { return materialise(s.pieces) }

func materialise(ps []piece) []byte {
	var b []byte
	for _, p := range ps {
		b = append(b, p.data...)
		b = append(b, make([]byte, p.zeros)...)
	}
	return b
}

// ignoredBlockHead starts a block body on track 126, which no film declares: demuxers drop it.
var ignoredBlockHead = []byte{0xFE, 0x00, 0x00, 0x80}

type spliceWalk struct {
	prefix    []byte
	s         *Splice
	video     uint64
	ts, last  int64
	haveTS    bool
	haveBlock bool
	closedAt  int // unknown-size Cluster closed by a level-1 element here (0 = not closed)
}

// PlanSplice completes the Cluster cut at clusterStart+len(prefix). prefix (> 0 bytes) starts on
// a Cluster and may span several. video is the film's video track; frameDur one frame in
// TimestampScale units.
func PlanSplice(prefix []byte, clusterStart int64, video, frameDur uint64) (*Splice, error) {
	v := clusterStart + int64(len(prefix))
	for {
		id, size, n, ok := readHeader(prefix)
		if !ok || id != idCluster {
			return nil, ErrStructure // includes a V that cuts the Cluster header itself
		}
		end := int64(-1)
		if size != unknownSize {
			if end = int64(n) + int64(size); end < int64(len(prefix)) { // whole Cluster served
				prefix, clusterStart = prefix[end:], clusterStart+end
				continue
			}
		}
		w := &spliceWalk{prefix: prefix, s: &Splice{V: v, ClusterStart: clusterStart}, video: video}
		rel, err := w.master(n, end, true)
		if err != nil {
			return nil, err
		}
		if w.closedAt > 0 {
			prefix, clusterStart = prefix[w.closedAt:], clusterStart+int64(w.closedAt)
			continue
		}
		w.s.End = clusterStart + rel
		w.s.HasTime = w.haveTS
		w.s.NextTime = uint64(max(w.ts, 0))
		if w.haveBlock {
			w.s.NextTime = uint64(max(w.ts+w.last+int64(frameDur), 0))
		}
		return w.s, nil
	}
}

// level1Prefix reports whether h starts with (a prefix of) a level-1 ID.
func level1Prefix(h []byte) bool {
	for _, id := range []uint64{idCluster, idCues, idTags, idChapters, idAttachments, idSeekHead, idInfo, idTracks, idEBML, idSegment} {
		ib := idBytes(id)
		if n := min(len(h), len(ib)); bytes.Equal(h[:n], ib[:n]) {
			return true
		}
	}
	return false
}

// level1 IDs close an unknown-size Cluster.
func level1(id uint64) bool {
	switch id {
	case idCluster, idCues, idTags, idChapters, idAttachments, idSeekHead, idInfo, idTracks, idEBML, idSegment:
		return true
	}
	return false
}

func (w *spliceWalk) emit(data []byte, zeros int64) {
	if len(data) > 0 || zeros > 0 {
		w.s.pieces = append(w.s.pieces, piece{data, zeros})
	}
}

// master walks the children of a master element whose body starts at p (relative to the
// prefix) and ends at end (-1 = unknown size), completes the one cut by V, then fills the rest
// with a Void. Returns the relative offset where the emitted bytes end.
func (w *spliceWalk) master(p int, end int64, cluster bool) (int64, error) {
	pre := w.prefix
	var rel int64
	for {
		if p == len(pre) { // V on a child boundary
			rel = int64(p)
			break
		}
		if end >= 0 && int64(p) >= end {
			return 0, ErrStructure // the prefix runs past this master
		}
		id, size, n, ok := readHeader(pre[p:])
		if ok && cluster && end < 0 && level1(id) {
			w.closedAt = p
			return 0, nil
		}
		if !ok && cluster && end < 0 && level1Prefix(pre[p:]) {
			return 0, ErrStructure // V cuts the header that closes this Cluster, not a child
		}
		if !ok { // V cuts this child's header
			room := int64(-1)
			if end >= 0 {
				room = end - int64(p)
			}
			e, err := w.completeHeader(pre[p:], room)
			if err != nil {
				return 0, err
			}
			rel = int64(p) + e
			break
		}
		if size == unknownSize {
			return 0, ErrStructure
		}
		cend := int64(p) + int64(n) + int64(size)
		if end >= 0 && cend > end {
			return 0, ErrStructure
		}
		if cend > int64(len(pre)) { // V inside this child's body
			switch {
			case id == idBlockGroup:
				if _, err := w.master(p+n, cend, false); err != nil {
					return 0, err
				}
			case (id == idSimpleBlock || id == idBlock) && len(pre) == p+n:
				w.blockBody(int64(size))
			default:
				w.emit(nil, cend-int64(len(pre)))
			}
			rel = cend
			break
		}
		if cluster {
			w.record(id, pre[p+n:cend])
		} else if id == idBlock {
			w.block(pre[p+n : cend]) // complete Block of the BlockGroup V cuts
		}
		p = int(cend)
	}
	if end < 0 {
		return rel, nil
	}
	switch rest := end - rel; {
	case rest == 1:
		return 0, ErrStructure // real children are >= 2 bytes; only a bad completion lands here
	case rest >= 2:
		w.emitVoid(rest)
	}
	return end, nil
}

func (w *spliceWalk) emitVoid(total int64) { w.emit(voidPiece(total)) }

// voidPiece is a Void element of exactly total bytes (>= 2): header data, zero body.
func voidPiece(total int64) ([]byte, int64) {
	for wd := 1; wd <= 8; wd++ {
		body := total - 1 - int64(wd)
		if body >= 0 && sizeLen(uint64(body)) <= wd {
			return appendElementHeader(nil, idVoid, uint64(body), wd), body
		}
	}
	panic("notice: void too small")
}

func (w *spliceWalk) blockBody(size int64) {
	if size >= int64(len(ignoredBlockHead)) {
		w.emit(ignoredBlockHead, size-int64(len(ignoredBlockHead)))
		return
	}
	w.emit(nil, size)
}

// record tracks the Cluster timestamp and the latest complete video block time.
func (w *spliceWalk) record(id uint64, body []byte) {
	switch id {
	case idTimestamp:
		w.ts, w.haveTS = int64(beUint(body)), true
	case idSimpleBlock:
		w.block(body)
	case idBlockGroup:
		children(body, func(id uint64, b []byte) {
			if id == idBlock {
				w.block(b)
			}
		})
	}
}

func (w *spliceWalk) block(b []byte) {
	l, ok := vintLen(b, 8)
	if !ok || len(b) < l+2 {
		return
	}
	track := beUint(b[:l]) &^ (uint64(1) << (7 * l))
	if track != w.video {
		return
	}
	t := int64(int16(uint16(b[l])<<8 | uint16(b[l+1])))
	if !w.haveBlock || t > w.last {
		w.last, w.haveBlock = t, true
	}
}

// completeHeader finishes an element whose header V cut: h holds its served bytes, room the
// bytes left in the enclosing master from the element start (-1 = unbounded). Returns the
// element's total length. Served bytes are kept; only the missing size bytes are chosen.
func (w *spliceWalk) completeHeader(h []byte, room int64) (int64, error) {
	idLen := leadLen(h[0])
	if idLen == 0 || idLen > 4 || len(h) < idLen {
		return 0, ErrStructure // the ID itself is cut: no element can be completed safely
	}
	var id uint64
	for _, b := range h[:idLen] {
		id = id<<8 | uint64(b)
	}
	minBody := int64(0)
	switch id {
	case idSimpleBlock, idBlock:
		minBody = int64(len(ignoredBlockHead))
	case idBlockGroup:
		minBody = 2
	}
	served := h[idLen:]
	var width int
	var body int64
	if len(served) == 0 { // width is free
		found := false
		for wd := 1; wd <= 8 && !found; wd++ {
			body = minBody
			if room >= 0 {
				body = room - int64(idLen) - int64(wd)
			}
			found = body >= minBody && sizeLen(uint64(body)) <= wd
			width = wd
		}
		if !found {
			return 0, ErrStructure
		}
		w.emit(appendSize(nil, uint64(body), width), 0)
	} else {
		if width = leadLen(served[0]); width == 0 {
			return 0, ErrStructure
		}
		free := width - len(served)
		hi := uint64(served[0] & (0xFF >> width))
		for _, b := range served[1:] {
			hi = hi<<8 | uint64(b)
		}
		lo := int64(hi << (8 * free))
		top := lo + int64(uint64(1)<<(8*free)) - 1
		if uint64(top) == uint64(1)<<(7*width)-1 {
			top-- // all ones means "unknown size"
		}
		hdr := int64(idLen + width)
		switch {
		case room < 0:
			body = max(lo, minBody)
		case room-hdr < lo:
			return 0, ErrStructure
		case room-hdr <= top:
			body = room - hdr
		default:
			body = top
			if room-hdr-body == 1 {
				body--
			}
		}
		if body < lo || body > top || body < minBody {
			return 0, ErrStructure
		}
		tail := make([]byte, free)
		for i := 0; i < free; i++ {
			tail[free-1-i] = byte(uint64(body) >> (8 * i))
		}
		w.emit(tail, 0)
	}
	switch id {
	case idSimpleBlock, idBlock:
		w.blockBody(body)
	case idBlockGroup:
		w.emitVoid(body)
	default:
		w.emit(nil, body)
	}
	return int64(idLen+width) + body, nil
}

// leadLen is the vint length announced by a first byte (0 for the invalid 0x00).
func leadLen(b byte) int {
	if b == 0 {
		return 0
	}
	l := 1
	for m := byte(0x80); b&m == 0; m >>= 1 {
		l++
	}
	return l
}
