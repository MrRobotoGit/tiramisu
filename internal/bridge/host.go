package bridge

import (
	"context"
	"io"

	"tiramisu/internal/streamer"
)

// SetAggressive switches the aggressive mode of a hydrated torrent: the engine records it and
// the streamer sizes the stream budgets from it (masterLimit 0: ConnectionsLimit).
func (c *NativeClient) SetAggressive(hash string, on bool, masterLimit int) {
	if c == nil || c.eng == nil {
		return
	}
	t := c.eng.Get(hash)
	if t == nil {
		return
	}
	t.SetAggressiveMode(on, masterLimit)
	src, ok := c.src.(engineSource)
	ts, ctl := t.Store(), t.Pieces()
	if !ok || ts == nil || ctl == nil {
		return
	}
	src.st.Torrent(ts, ctl).SetAggressive(on, masterLimit)
}

// ReadRange reads len(p) bytes at off of a file in-process (cue splitting): it activates the
// torrent as GET /stream did, then reads a demand stream. n < len(p) only at the end of file.
func (c *NativeClient) ReadRange(ctx context.Context, hash string, fileID int, off int64, p []byte) (int, error) {
	if err := c.Wake(ctx, hash, fileID); err != nil {
		return 0, err
	}
	w := &sliceWriter{p: p}
	err := c.WriteStreamAs(ctx, hash, fileID, off, int64(len(p)), streamer.KindDemand, w)
	return w.n, err
}

type sliceWriter struct {
	p []byte
	n int
}

func (w *sliceWriter) Write(b []byte) (int, error) {
	n := copy(w.p[w.n:], b)
	w.n += n
	if n < len(b) {
		return n, io.ErrShortBuffer
	}
	return n, nil
}
