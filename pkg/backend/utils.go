package backend

import (
	"context"
	"path/filepath"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// LatestFile returns the contents of the latest file at the specified path in
// the repository and its file path. Its git commands stop when ctx is done.
func LatestFile(ctx context.Context, r proto.Repository, ref *git.Reference, pattern string) (string, string, error) {
	repo, err := r.Open()
	if err != nil {
		return "", "", err
	}
	return git.LatestFile(repo.WithContext(ctx), ref, pattern)
}

// Readme returns the repository's README.
// It searches in priority order: root, .github, docs. Its git commands stop
// when ctx is done.
func Readme(ctx context.Context, r proto.Repository, ref *git.Reference) (readme string, path string, err error) {
	for _, dir := range []string{".", ".github", "docs"} {
		readme, path, err = LatestFile(ctx, r, ref, filepath.Join(dir, "[rR][eE][aA][dD][mM][eE]*"))
		if err == nil {
			return
		}
	}
	return
}
