package backend

import (
	"errors"
	"syscall"
)

// isInvalidName reports whether a stat error means the caller-supplied name
// cannot name a path on this filesystem at all: it is too long, contains a
// NUL byte, or runs through a file where a directory should be. No
// repository can exist there, and such names come straight from requests, so
// they are not worth an error log line.
func isInvalidName(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTDIR) ||
		isInvalidNamePlatform(err)
}
