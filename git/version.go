package git

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/aymanbagabas/git-module"
)

// ErrVersionUnparseable is returned when `git version` output cannot be read.
var ErrVersionUnparseable = errors.New("cannot parse git version")

// Version is a parsed git version.
type Version struct {
	Major, Minor, Patch int
}

// String implements fmt.Stringer.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// versionRe matches the numeric prefix of a git version, with or without the
// "git version " banner. Whatever follows (rc, vendor, or platform suffixes
// such as "(Apple Git-146)", ".windows.1", "-rc0") is ignored.
var versionRe = regexp.MustCompile(`^(?:git version )?(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion parses the output of `git version` or a bare version string.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrVersionUnparseable, s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

// BinVersion returns the version of the git binary in use. It runs
// `git version` on every call and keeps nothing: git-module's own BinVersion
// remembers its first result in a sync.Once, failures included, so a single
// passing failure (fork EAGAIN under load) would stick until restart. Callers
// that want to cache a successful result do so themselves.
func BinVersion() (Version, error) {
	out, err := git.NewCommand("version").Run()
	if err != nil {
		return Version{}, fmt.Errorf("git version: %w", err)
	}
	return ParseVersion(string(out))
}
