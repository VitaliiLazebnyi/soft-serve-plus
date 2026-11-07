package backend

import (
	gopath "path"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// LatestFile returns the contents of the latest file at the specified path in
// the repository and its file path.
func LatestFile(r proto.Repository, ref *git.Reference, pattern string) (string, string, error) {
	repo, err := r.Open()
	if err != nil {
		return "", "", err
	}
	return git.LatestFile(repo, ref, pattern)
}

// Readme returns the repository's README.
// It searches in priority order: root, .github, docs. Patterns are git
// paths, so they are joined with forward slashes on every OS.
func Readme(r proto.Repository, ref *git.Reference) (readme string, path string, err error) {
	for _, dir := range []string{".", ".github", "docs"} {
		readme, path, err = LatestFile(r, ref, gopath.Join(dir, "[rR][eE][aA][dD][mM][eE]*"))
		if err == nil {
			return
		}
	}
	return
}
