package git

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"
)

// maxSymlinkHops is how many symlinks LatestFile follows from a matching
// entry before it gives up on it, which also stops a symlink loop.
const maxSymlinkHops = 8

// maxSymlinkTarget is the largest symlink target, in bytes, LatestFile reads.
const maxSymlinkTarget = 4096

// LatestFile returns the contents of the first text file at the specified
// path pattern in the repository and its file path. The pattern has
// path.Match syntax, and only its last element may hold wildcards. Binary
// files and files larger than maxSize bytes are skipped; when only files too
// large match, an error wrapping ErrFileTooLarge is returned. A maxSize of 0
// or less means no limit.
// A matching symlink is followed, through further symlinks, to the file it
// points to within the repository, and the returned path is that file's.
// Matching directories and submodules, and symlinks that do not lead to a
// file, are skipped.
func LatestFile(repo *Repository, ref *Reference, pattern string, maxSize int64) (string, string, error) {
	if _, err := path.Match(pattern, ""); err != nil {
		return "", "", fmt.Errorf("pattern %q: %w", pattern, err)
	}
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
	// In a directory too large to list, look among the entries listed.
	ents, err := t.Entries()
	if err != nil && !errors.Is(err, ErrTreeTruncated) {
		return "", "", err
	}
	var root *Tree
	var tooLarge error
	for _, te := range ents {
		fp := path.Join(dir, te.Name())
		if te.IsTree() || te.IsCommit() {
			continue
		}
		if ok, _ := path.Match(pattern, fp); !ok {
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
		bts, err := readText(te.File(), maxSize)
		switch {
		case errors.Is(err, errBinary):
			continue
		case errors.Is(err, ErrFileTooLarge):
			// A later match may still fit.
			tooLarge = err
			continue
		case err != nil:
			return "", "", err
		}
		return string(bts), fp, nil
	}
	if tooLarge != nil {
		return "", "", tooLarge
	}
	return "", "", ErrFileNotFound
}

// errBinary is returned by readText for a binary file.
var errBinary = errors.New("binary file")

// readText reads f with one git process. It returns errBinary, having read
// no more than sniffLen bytes, when the file is binary, and an error
// wrapping ErrFileTooLarge when it is larger than maxSize bytes; a maxSize
// of 0 or less means no limit. A file listed as too large is only sniffed,
// so a binary one is still told apart from a text one.
func readText(f *File, maxSize int64) ([]byte, error) {
	w := &textWriter{limit: maxSize}
	listedTooLarge := maxSize > 0 && f.Size() > maxSize
	if listedTooLarge {
		w.limit = sniffLen
	}
	var stderr bytes.Buffer
	err := f.Pipeline(w, &stderr)
	switch {
	case w.binary:
		return nil, errBinary
	case listedTooLarge:
		return nil, fmt.Errorf("%w: %d bytes, limit is %d", ErrFileTooLarge, f.Size(), maxSize)
	case w.over:
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrFileTooLarge, maxSize)
	case err != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w - %s", err, msg)
		}
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// textWriter buffers a file, failing, which stops the git process feeding
// it, as soon as a NUL in the first sniffLen bytes shows the file is binary
// or the file grows past limit bytes (no limit when limit <= 0).
type textWriter struct {
	buf    bytes.Buffer
	limit  int64
	binary bool
	over   bool
}

func (w *textWriter) Write(p []byte) (int, error) {
	if n := w.buf.Len(); n < sniffLen && bytes.IndexByte(p[:min(len(p), sniffLen-n)], 0) >= 0 {
		w.binary = true
		return 0, errBinary
	}
	if w.limit > 0 && int64(w.buf.Len())+int64(len(p)) > w.limit {
		w.over = true
		return 0, ErrFileTooLarge
	}
	return w.buf.Write(p)
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
