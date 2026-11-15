package git

import (
	"regexp"

	"github.com/aymanbagabas/git-module"
)

// ZeroID is the zero hash.
const ZeroID = git.EmptyID

var zeroHashPattern = regexp.MustCompile(`^0{40,}$`)

// IsZeroHash returns whether the hash is a zero hash.
func IsZeroHash(h string) bool {
	return zeroHashPattern.MatchString(h)
}

// Commit is a wrapper around git.Commit with helper methods.
type Commit = git.Commit

// Commits is a list of commits.
type Commits []*Commit
