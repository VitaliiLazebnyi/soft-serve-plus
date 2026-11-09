package git

import (
	"errors"
	"os"
	"path"
	"path/filepath"

	"github.com/gobwas/glob"
)

// LatestFile returns the contents of the first text file at the specified
// path pattern in the repository and its file path. Binary files and files
// larger than maxSize bytes are skipped; when only files too large match, an
// error wrapping ErrFileTooLarge is returned. A maxSize of 0 or less means no
// limit.
func LatestFile(repo *Repository, ref *Reference, pattern string, maxSize int64) (string, string, error) {
	g := glob.MustCompile(pattern)
	dir := path.Dir(pattern)
	if ref == nil {
		head, err := repo.HEAD()
		if err != nil {
			return "", "", err
		}
		ref = head
	}
	t, err := repo.TreePath(ref, dir)
	if err != nil {
		return "", "", err
	}
	ents, err := t.Entries()
	if err != nil {
		return "", "", err
	}
	var tooLarge error
	for _, e := range ents {
		te := e
		fp := path.Join(dir, te.Name())
		if te.IsTree() {
			continue
		}
		if g.Match(fp) {
			if te.IsSymlink() {
				bts, err := te.File().ContentsLimit(maxSize)
				if err != nil {
					return "", "", err
				}
				fp = string(bts)
				te, err = t.TreeEntry(fp)
				if err != nil {
					return "", "", err
				}
			}
			f := te.File()
			if bin, err := f.IsBinary(); err != nil {
				return "", "", err
			} else if bin {
				continue
			}
			bts, err := f.ContentsLimit(maxSize)
			if errors.Is(err, ErrFileTooLarge) {
				// A later match may still fit.
				tooLarge = err
				continue
			}
			if err != nil {
				return "", "", err
			}
			return string(bts), fp, nil
		}
	}
	if tooLarge != nil {
		return "", "", tooLarge
	}
	return "", "", ErrFileNotFound
}

// Returns true if path is a directory containing an `objects` directory and a
// `HEAD` file.
func isGitDir(path string) bool {
	stat, err := os.Stat(filepath.Join(path, "objects"))
	if err != nil {
		return false
	}
	if !stat.IsDir() {
		return false
	}

	stat, err = os.Stat(filepath.Join(path, "HEAD"))
	if err != nil {
		return false
	}
	if stat.IsDir() {
		return false
	}

	return true
}
