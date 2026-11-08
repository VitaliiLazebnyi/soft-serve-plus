package git

import (
	"errors"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, which syscall does not
// define.
const errSharingViolation = syscall.Errno(32)

// isRenameRetryable reports whether a failed rename is likely caused by
// another process holding the target open, which goes away once it closes it.
func isRenameRetryable(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errSharingViolation)
}
