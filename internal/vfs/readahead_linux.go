//go:build linux

package vfs

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// SetMountReadAhead sets the kernel readahead of the FUSE mount at dir. go-fuse's MaxReadAhead can
// only lower the kernel's 128KB default, so 1MB reads (max_read) need the bdi knob. Writing it
// needs root: as an unprivileged user the service unit sets it instead. kb <= 0 leaves it alone.
func SetMountReadAhead(dir string, kb int) {
	if kb <= 0 {
		return
	}
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return
	}
	p := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)))
	if cur, err := os.ReadFile(p); err == nil {
		if v, err := strconv.Atoi(string(trimNL(cur))); err == nil && v >= kb {
			return
		}
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(kb)), 0); err != nil {
		log.Printf("[FUSE] readahead not set to %dKB (%v): set it as root in %s", kb, err, p)
		return
	}
	log.Printf("[FUSE] readahead set to %dKB", kb)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
