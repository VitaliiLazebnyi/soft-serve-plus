package git

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gobwas/glob"
)

// maxSymlinkHops is how many symlinks LatestFile follows from a matching
// entry before it gives up on it, which also stops a symlink loop.
const maxSymlinkHops = 8

// maxSymlinkTarget is the largest symlink target, in bytes, LatestFile reads.
const maxSymlinkTarget = 4096

// LatestFile returns the contents of the first text file at the specified
// path pattern in the repository and its file path. Binary files and files
// larger than maxSize bytes are skipped; when only files too large match, an
// error wrapping ErrFileTooLarge is returned. A maxSize of 0 or less means no
// limit.
// A matching symlink is followed, through further symlinks, to the file it
// points to within the repository, and the returned path is that file's.
// Matching directories and submodules, and symlinks that do not lead to a
// file, are skipped.
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
	var root *Tree
	var tooLarge error
	for _, te := range ents {
		fp := path.Join(dir, te.Name())
		if te.IsTree() || te.IsCommit() || !g.Match(fp) {
			continue
		}
		if te.IsSymlink() {
			if root == nil {
				root, err = repo.Tree(ref)
				if err != nil {
					return "", "", err
				}
			}
			te, fp = resolveSymlink(root, te, fp)
			if te == nil {
				continue
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
	if tooLarge != nil {
		return "", "", tooLarge
	}
	return "", "", ErrFileNotFound
}

// resolveSymlink follows the symlink te at fp, and any symlinks it leads
// to, to a file in root. Targets are relative to the symlink's directory.
// It returns the file's entry and path, or a nil entry when the chain is too
// long or leads out of the repository, to nothing, or to something other
// than a file.
func resolveSymlink(root *Tree, te *TreeEntry, fp string) (*TreeEntry, string) {
	for range maxSymlinkHops {
		// A symlink's target is a path, so a blob larger than any path
		// is not one worth reading.
		target, err := te.File().ContentsLimit(maxSymlinkTarget)
		if err != nil {
			return nil, ""
		}
		link := string(target)
		if path.IsAbs(link) {
			return nil, ""
		}
		fp = path.Join(path.Dir(fp), link)
		if fp == ".." || strings.HasPrefix(fp, "../") {
			return nil, ""
		}
		te, err = root.TreeEntry(fp)
		if err != nil || te.IsTree() || te.IsCommit() {
			return nil, ""
		}
		if !te.IsSymlink() {
			return te, fp
		}
	}
	return nil, ""
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
