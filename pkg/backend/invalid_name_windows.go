//go:build windows

package backend

import (
	"errors"
	"syscall"
)

// Windows error codes for a path that is malformed or too long; the syscall
// package does not name them.
const (
	errorFilenameExcedRange syscall.Errno = 206
	errorBadPathname        syscall.Errno = 161
	errorInvalidName        syscall.Errno = 123
)

func isInvalidNamePlatform(err error) bool {
	return errors.Is(err, errorInvalidName) ||
		errors.Is(err, errorBadPathname) ||
		errors.Is(err, errorFilenameExcedRange)
}
