package lfs

import (
	"errors"
	"path"
	"strings"
)

// MaxLockPathLength is the longest lock path, in bytes, a server accepts.
const MaxLockPathLength = 4096

// ErrInvalidLockPath is returned by ValidateLockPath for a path that cannot
// name a file in a repository.
var ErrInvalidLockPath = errors.New("invalid lock path")

// ValidateLockPath checks that p can name a file in a repository's working
// tree, as Git LFS sends it: relative to the repository root, separated by
// forward slashes and in its clean form. It refuses an empty or overlong
// path, an absolute one, one holding a NUL byte, and one with an empty, "."
// or ".." element, so two spellings of one file cannot hold separate locks
// and a lock cannot name a file outside the repository.
func ValidateLockPath(p string) error {
	switch {
	case p == "", len(p) > MaxLockPathLength:
		return ErrInvalidLockPath
	case strings.HasPrefix(p, "/"), strings.ContainsRune(p, 0):
		return ErrInvalidLockPath
	case path.Clean(p) != p, p == ".", p == "..", strings.HasPrefix(p, "../"):
		return ErrInvalidLockPath
	}
	return nil
}
