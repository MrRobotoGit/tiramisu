package torr

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// streamBufferSize batches http.ServeContent's 32KB reads: every torrent reader Read takes the
// client lock in write mode, so larger reads take it far less often.
const streamBufferSize = 1 << 20

// bufferedStreamReader is a ReadSeeker over a bufio.Reader. Backported from TorrServer #900.
type bufferedStreamReader struct {
	source io.ReadSeeker
	buffer *bufio.Reader
}

func newBufferedStreamReader(source io.ReadSeeker, size int) *bufferedStreamReader {
	return &bufferedStreamReader{source: source, buffer: bufio.NewReaderSize(source, size)}
}

func (r *bufferedStreamReader) Read(p []byte) (int, error) { return r.buffer.Read(p) }

func (r *bufferedStreamReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekCurrent {
		// The source has advanced past unread buffered bytes.
		offset -= int64(r.buffer.Buffered())
	}
	pos, err := r.source.Seek(offset, whence)
	if err == nil {
		r.buffer.Reset(r.source)
	}
	return pos, err
}

// streamContent buffers content unless the request asks for a single range shorter than the
// buffer: a fill would read past the range end, and a short fetch gains nothing from batching.
// content must stay the context-bound reader, so the stream timeout still bounds every Read.
func streamContent(content io.ReadSeeker, rangeHeader string) io.ReadSeeker {
	if n, ok := singleRangeLength(rangeHeader); ok && n < streamBufferSize {
		return content
	}
	return newBufferedStreamReader(content, streamBufferSize)
}

// singleRangeLength returns the length of a closed or suffix single byte range.
func singleRangeLength(h string) (int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, false
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok || last == "" {
		return 0, false
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < 0 {
		return 0, false
	}
	if first == "" {
		return end, true // suffix range: the last `end` bytes
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || end < start {
		return 0, false
	}
	return end - start + 1, true
}
