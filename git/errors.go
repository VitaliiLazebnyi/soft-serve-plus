package git

import (
	"errors"

	"github.com/aymanbagabas/git-module"
)

var (
	// ErrFileNotFound is returned when a file is not found.
	ErrFileNotFound = errors.New("file not found")
	// ErrReferenceNotExist is returned when a reference does not exist.
	ErrReferenceNotExist = git.ErrReferenceNotExist
	// ErrRevisionNotExist is returned when a revision is not found.
	ErrRevisionNotExist = git.ErrRevisionNotExist
	// ErrMalformedCommit is returned for a commit object that cannot be
	// parsed, such as one whose author has no email or no date.
	ErrMalformedCommit = errors.New("malformed commit")
	// ErrNotAGitRepository is returned when the given path is not a Git repository.
	ErrNotAGitRepository = errors.New("not a git repository")
	// ErrFileTooLarge is returned when a file is larger than the size limit
	// it is read with.
	ErrFileTooLarge = errors.New("file too large")
	// ErrTreeTruncated is returned along with the first entries that fit
	// of a directory too large to list in full (see Tree.Entries).
	ErrTreeTruncated = errors.New("tree listing truncated")
	// ErrMalformedTree is returned for a tree listing that cannot be
	// parsed.
	ErrMalformedTree = errors.New("malformed tree")
)
