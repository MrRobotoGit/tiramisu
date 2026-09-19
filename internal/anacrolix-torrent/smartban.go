package torrent

import (
	"net/netip"

	g "github.com/anacrolix/generics"

	"github.com/anacrolix/torrent/smartban"
)

type bannableAddr = netip.Addr

type smartBanCache = smartban.Cache[bannableAddr, RequestIndex, uint64]

type blockCheckingWriter struct {
	cache        *smartBanCache
	requestIndex RequestIndex
	// Peers that didn't match blocks written now.
	badPeers map[bannableAddr]struct{}
	// Chunk-sized buffer reused for every block of the piece. Backported from anacrolix/torrent
	// upstream (commit 4f0e00d): the previous bytes.Buffer grew to the size of the largest single
	// write and was thrown away per piece.
	chunkBuffer []byte
	bufferUsed  int
}

func (me *blockCheckingWriter) chunkSize() int {
	return len(me.chunkBuffer)
}

func (me *blockCheckingWriter) checkBufferedBlock() {
	if me.bufferUsed == 0 {
		panic("checkBufferedBlock without buffered data")
	}
	b := me.chunkBuffer[:me.bufferUsed]
	me.bufferUsed = 0
	me.checkBlock(b)
}

func (me *blockCheckingWriter) checkBlock(b []byte) {
	for _, peer := range me.cache.CheckBlock(me.requestIndex, b) {
		g.MakeMapIfNilAndSet(&me.badPeers, peer, struct{}{})
	}
	me.requestIndex++
}

func (me *blockCheckingWriter) finishPartialBlock(b []byte) int {
	if me.bufferUsed == 0 {
		return 0
	}
	n := copy(me.chunkBuffer[me.bufferUsed:], b)
	me.bufferUsed += n
	if me.bufferUsed >= me.chunkSize() {
		me.checkBufferedBlock()
	}
	return n
}

func (me *blockCheckingWriter) Write(b []byte) (n int, err error) {
	n = me.finishPartialBlock(b)
	b = b[n:]
	if len(b) == 0 {
		return
	}
	for len(b) >= me.chunkSize() {
		me.checkBlock(b[:me.chunkSize()])
		b = b[me.chunkSize():]
		n += me.chunkSize()
	}
	if me.bufferUsed != 0 {
		panic("buffer not empty before buffering the tail")
	}
	me.bufferUsed = copy(me.chunkBuffer, b)
	n += me.bufferUsed
	return n, err
}

// Check any remaining block data. Terminal pieces or piece sizes that don't divide into the chunk
// size cleanly may leave fragments that should be checked.
func (me *blockCheckingWriter) Flush() {
	if me.bufferUsed != 0 {
		me.checkBufferedBlock()
	}
	if me.bufferUsed != 0 {
		panic("flush left buffered data")
	}
}
