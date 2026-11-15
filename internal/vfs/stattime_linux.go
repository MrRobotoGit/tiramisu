//go:build linux

package vfs

import (
	"syscall"
	"time"
)

// StatTimes returns the modification and change times recorded in st.
func StatTimes(st *syscall.Stat_t) (mtime, ctime time.Time) {
	return time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)), time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
}
