package vfs

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// ToErrno translates various internal and standard errors into POSIX syscall.Errno codes.
// This improves "transparency" for the operating system and media players.
func ToErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	// Unwrap error if possible
	unwrapped := errors.Unwrap(err)
	if unwrapped != nil {
		return ToErrno(unwrapped)
	}

	// 1. Direct syscall.Errno
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}

	// 2. Standard OS Errors
	if os.IsNotExist(err) {
		return syscall.ENOENT
	}
	if os.IsPermission(err) {
		return syscall.EPERM
	}
	if os.IsTimeout(err) {
		return syscall.ETIMEDOUT
	}

	// 3. Network / HTTP Errors
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return syscall.ETIMEDOUT
	}

	// 4. Special cases
	if errors.Is(err, io.EOF) {
		return 0 // EOF is not an error in many read contexts
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return syscall.EIO
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return syscall.EPIPE
	}

	// 5. Default fallback
	return syscall.EIO
}
