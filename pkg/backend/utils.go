package backend

import (
	"context"
	"errors"
	pathpkg "path"

	"github.com/charmbracelet/soft-serve/git"
)

// Opener is a repository LatestFile and Readme can read: they only open it.
type Opener interface {
	Open() (*git.Repository, error)
}

// LatestFile returns the contents of the latest file at the specified path in
// the repository and its file path. Files larger than maxSize bytes are not
// read (see git.LatestFile). Its git commands stop when ctx is done.
func LatestFile(ctx context.Context, r Opener, ref *git.Reference, pattern string, maxSize int64) (string, string, error) {
	repo, err := r.Open()
	if err != nil {
		return "", "", err
	}
	return git.LatestFile(repo.WithContext(ctx), ref, pattern, maxSize)
}

// Readme returns the repository's README and its path. It looks in the
// root, .github and docs, in that order, and returns the first README it can
// show. A README larger than maxSize bytes is not read and the search goes
// on; 0 or less means no limit. With no README to show, the error wraps
// git.ErrFileTooLarge if one was too large, and git.ErrFileNotFound
// otherwise. Any other error, such as a git failure, ends the search and is
// returned. Its git commands stop when ctx is done.
func Readme(ctx context.Context, r Opener, ref *git.Reference, maxSize int64) (string, string, error) {
	var tooLarge error
	for _, dir := range []string{".", ".github", "docs"} {
		// Tree paths always use forward slashes, whatever the OS.
		readme, path, err := LatestFile(ctx, r, ref, pathpkg.Join(dir, "[rR][eE][aA][dD][mM][eE]*"), maxSize)
		switch {
		case err == nil:
			return readme, path, nil
		case errors.Is(err, git.ErrFileTooLarge):
			if tooLarge == nil {
				tooLarge = err
			}
		case errors.Is(err, git.ErrFileNotFound), errors.Is(err, git.ErrRevisionNotExist):
			// No README here, or no such directory.
		default:
			return "", "", err
		}
	}
	if tooLarge != nil {
		return "", "", tooLarge
	}
	return "", "", git.ErrFileNotFound
}
