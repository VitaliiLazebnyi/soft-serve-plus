package backend

import (
	pathpkg "path"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// LatestFile returns the contents of the latest file at the specified path in
// the repository and its file path. Files larger than maxSize bytes are not
// read (see git.LatestFile).
func LatestFile(r proto.Repository, ref *git.Reference, pattern string, maxSize int64) (string, string, error) {
	repo, err := r.Open()
	if err != nil {
		return "", "", err
	}
	return git.LatestFile(repo, ref, pattern, maxSize)
}

// Readme returns the repository's README.
// It searches in priority order: root, .github, docs. A README larger than
// maxSize bytes is not read; 0 or less means no limit.
func Readme(r proto.Repository, ref *git.Reference, maxSize int64) (readme string, path string, err error) {
	for _, dir := range []string{".", ".github", "docs"} {
		// Tree paths always use forward slashes, whatever the OS.
		readme, path, err = LatestFile(r, ref, pathpkg.Join(dir, "[rR][eE][aA][dD][mM][eE]*"), maxSize)
		if err == nil {
			return
		}
	}
	return
}
