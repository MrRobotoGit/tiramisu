//go:build darwin

package vfs

import (
	"syscall"
	"time"
)

// StatTimes returns the modification and change times recorded in st.
func StatTimes(st *syscall.Stat_t) (mtime, ctime time.Time) {
	return time.Unix(st.Mtimespec.Sec, st.Mtimespec.Nsec), time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
