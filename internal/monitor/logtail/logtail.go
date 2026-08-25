// Package logtail reads the last bytes of a file without loading the whole thing.
package logtail

import (
	"io"
	"os"
)

// Read returns the last max bytes of the file at path, or everything when it is shorter.
// os.ReadFile followed by a re-slice looks equivalent and is not: it pulls the entire file
// into the heap, and the tail slice keeps that whole array alive because it still points into
// it. On a 218MB log that is 218MB resident to keep 256KB - the largest single allocation in
// the process. Returns nil on any error; the callers treat a missing log as no data.
func Read(path string, max int64) []byte {
	if max <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	if size <= 0 {
		return nil
	}
	off := int64(0)
	if size > max {
		off, size = size-max, max
	}

	buf := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(f, off, size), buf); err != nil {
		return nil
	}
	return buf
}
