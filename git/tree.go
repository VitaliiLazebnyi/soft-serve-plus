package git

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	gopath "path"
	"sort"
	"strconv"
	"strings"

	"github.com/aymanbagabas/git-module"
)

// Tree is a wrapper around git.Tree with helper methods.
type Tree struct {
	*git.Tree
	Path       string
	Repository *Repository
}

// TreeEntry is a wrapper around git.TreeEntry with helper methods.
type TreeEntry struct {
	*git.TreeEntry
	// path is the full path of the file
	path string
	// repo is the repository the entry was read from. Its context bounds
	// reading the entry's contents. It is nil for entries built elsewhere.
	repo *Repository
	// size is the object size, valid when sized is set. See
	// Tree.EntriesWithSizes.
	size  int64
	sized bool
}

// Entries is a wrapper around git.Entries.
type Entries []*TreeEntry

var sorters = []func(t1, t2 *TreeEntry) bool{
	func(t1, t2 *TreeEntry) bool {
		return (t1.IsTree() || t1.IsCommit()) && !t2.IsTree() && !t2.IsCommit()
	},
	func(t1, t2 *TreeEntry) bool {
		return t1.Name() < t2.Name()
	},
}

// Len implements sort.Interface.
func (es Entries) Len() int { return len(es) }

// Swap implements sort.Interface.
func (es Entries) Swap(i, j int) { es[i], es[j] = es[j], es[i] }

// Less implements sort.Interface.
func (es Entries) Less(i, j int) bool {
	t1, t2 := es[i], es[j]
	var k int
	for k = 0; k < len(sorters)-1; k++ {
		sorter := sorters[k]
		switch {
		case sorter(t1, t2):
			return true
		case sorter(t2, t1):
			return false
		}
	}
	return sorters[k](t1, t2)
}

// Sort sorts the entries in the tree.
func (es Entries) Sort() {
	sort.Sort(es)
}

// File is a wrapper around git.Blob with helper methods.
type File struct {
	*git.Blob
	Entry *TreeEntry
}

// Name returns the name of the file.
func (f *File) Name() string {
	return f.Entry.Name()
}

// Path returns the full path of the file.
func (f *File) Path() string {
	return f.Entry.path
}

// SubTree returns the sub-tree at the given path.
func (t *Tree) SubTree(path string) (*Tree, error) {
	tree, err := t.Subtree(path, t.lsTreeOpts())
	if err != nil {
		return nil, err
	}
	return &Tree{
		Tree:       tree,
		Path:       path,
		Repository: t.Repository,
	}, nil
}

// Entries returns the entries in the tree.
func (t *Tree) Entries() (Entries, error) {
	entries, err := t.Tree.Entries(t.lsTreeOpts())
	if err != nil {
		return nil, err
	}
	ret := make(Entries, len(entries))
	for i, e := range entries {
		ret[i] = &TreeEntry{
			TreeEntry: e,
			path:      gopath.Join(t.Path, e.Name()),
			repo:      t.Repository,
		}
	}
	return ret, nil
}

// EntriesWithSizes returns the entries in the tree with their sizes loaded
// by one git process for the whole tree. Calling Size on an entry from
// Entries runs a git process for that entry alone, which adds up when a
// listing shows the size of every entry.
func (t *Tree) EntriesWithSizes() (Entries, error) {
	ents, err := t.Entries()
	if err != nil {
		return nil, err
	}

	var in bytes.Buffer
	for _, e := range ents {
		if e.IsBlob() || e.IsExec() || e.IsSymlink() {
			in.WriteString(e.ID().String())
			in.WriteByte('\n')
		}
	}

	sizes := make(map[string]int64)
	if in.Len() > 0 {
		var out, stderr bytes.Buffer
		if err := git.NewCommandWithContext(t.Repository.Context(), "cat-file", "--batch-check=%(objectname) %(objectsize)").
			RunInDirWithOptions(t.Repository.Path, git.RunInDirOptions{
				Stdin:  &in,
				Stdout: &out,
				Stderr: &stderr,
			}); err != nil {
			return nil, fmt.Errorf("cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		// A missing object is reported as "<id> missing"; it is left
		// out so its size reads as zero, which is what Size gives it.
		for line := range strings.Lines(out.String()) {
			id, size, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			if n, err := strconv.ParseInt(size, 10, 64); err == nil {
				sizes[id] = n
			}
		}
	}

	for _, e := range ents {
		e.size = sizes[e.ID().String()]
		e.sized = true
	}
	return ents, nil
}

// Size returns the size of the entry's object, or zero for a tree or a
// submodule. Entries from EntriesWithSizes already hold it; any other entry
// asks git.
func (e *TreeEntry) Size() int64 {
	if e.sized {
		return e.size
	}
	return e.TreeEntry.Size()
}

// TreeEntry returns the TreeEntry for the file path, which is relative to
// the tree and may name a file in a subdirectory.
func (t *Tree) TreeEntry(path string) (*TreeEntry, error) {
	entry, err := t.Tree.TreeEntry(path, t.lsTreeOpts())
	if err != nil {
		return nil, err
	}
	return &TreeEntry{
		TreeEntry: entry,
		path:      gopath.Join(t.Path, path),
		repo:      t.Repository,
	}, nil
}

// lsTreeOpts returns options that run ls-tree under the context of the
// tree's repository.
func (t *Tree) lsTreeOpts() git.LsTreeOptions {
	return git.LsTreeOptions{CommandOptions: t.Repository.cmdOpts()}
}

const sniffLen = 8000

// IsBinary detects if data is a binary value based on:
// http://git.kernel.org/cgit/git/git.git/tree/xdiff-interface.c?id=HEAD#n198
func IsBinary(r io.Reader) (bool, error) {
	reader := bufio.NewReader(r)
	c := 0
	for c < sniffLen {
		b, err := reader.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}

		if b == byte(0) {
			return true, nil
		}

		c++
	}

	return false, nil
}

// Pipeline writes the contents of the file to stdout and git's errors to
// stderr. Unlike git-module's Blob.Pipeline it runs under the context of the
// repository the file was read from.
func (f *File) Pipeline(stdout, stderr io.Writer) error {
	if f.Entry == nil || f.Entry.repo == nil {
		return f.Blob.Pipeline(stdout, stderr)
	}
	r := f.Entry.repo
	return git.NewCommandWithContext(r.Context(), "show", f.ID().String()).
		RunInDirPipeline(stdout, stderr, r.Path)
}

// IsBinary returns true if the file is binary. Only the first sniffLen bytes
// of the file are read.
func (f *File) IsBinary() (bool, error) {
	w := &sniffWriter{}
	err := f.Pipeline(w, io.Discard)
	if err != nil && !w.full() {
		return false, err
	}
	// Once enough bytes are in, git is stopped by the write error, so err
	// does not matter.
	return IsBinary(bytes.NewReader(w.buf))
}

// errSniffDone stops a blob read once sniffWriter has enough bytes.
var errSniffDone = errors.New("sniffed enough bytes")

// sniffWriter keeps the first sniffLen bytes written to it and then fails,
// which stops the git process feeding it.
type sniffWriter struct {
	buf []byte
}

func (w *sniffWriter) full() bool { return len(w.buf) >= sniffLen }

func (w *sniffWriter) Write(p []byte) (int, error) {
	n := min(len(p), sniffLen-len(w.buf))
	w.buf = append(w.buf, p[:n]...)
	if n < len(p) {
		return n, errSniffDone
	}
	return n, nil
}

// Mode returns the mode of the file in fs.FileMode format. Git modes are not
// fs.FileMode bits, so each kind of entry is mapped to its equivalent: a
// symlink is fs.ModeSymlink, and a submodule, which has no equivalent, is
// fs.ModeIrregular.
func (e *TreeEntry) Mode() fs.FileMode {
	switch e.TreeEntry.Mode() {
	case git.EntryTree:
		return fs.ModeDir | fs.ModePerm
	case git.EntryBlob:
		return 0o644
	case git.EntryExec:
		return 0o755
	case git.EntrySymlink:
		return fs.ModeSymlink | fs.ModePerm
	default:
		return fs.ModeIrregular
	}
}

// File returns the file for the TreeEntry.
func (e *TreeEntry) File() *File {
	b := e.Blob()
	return &File{
		Blob:  b,
		Entry: e,
	}
}

// Contents returns the contents of the file.
func (e *TreeEntry) Contents() ([]byte, error) {
	return e.File().Contents()
}

// Contents returns the contents of the file.
func (f *File) Contents() ([]byte, error) {
	if f.Entry == nil || f.Entry.repo == nil {
		return f.Blob.Bytes()
	}
	if err := f.Entry.repo.Context().Err(); err != nil {
		return nil, err
	}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	// Preallocate the buffer, as git-module's Blob.Bytes does, so that a big
	// file is not read through repeated doubling. Size runs a short
	// `cat-file -s` without a context (git-module caches the size per entry
	// and this version does not take it from ls-tree), which is why the
	// context is checked first.
	if size := f.Blob.Size(); size > 0 {
		stdout.Grow(int(size))
	}
	if err := f.Pipeline(stdout, stderr); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w - %s", err, stderr.String())
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// ContentsLimit returns the contents of the file, or an error wrapping
// ErrFileTooLarge without reading it when the file is larger than limit
// bytes. A limit of 0 or less means no limit.
func (f *File) ContentsLimit(limit int64) ([]byte, error) {
	if limit <= 0 {
		return f.Contents()
	}
	if size := f.Size(); size > limit {
		return nil, fmt.Errorf("%w: %d bytes, limit is %d", ErrFileTooLarge, size, limit)
	}
	// Size is looked up separately and is 0 when the lookup fails, so cap
	// the read as well.
	w := &limitWriter{limit: limit}
	stderr := new(bytes.Buffer)
	if err := f.Pipeline(w, stderr); err != nil {
		if w.over {
			return nil, fmt.Errorf("%w: limit is %d bytes", ErrFileTooLarge, limit)
		}
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w - %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// limitWriter buffers up to limit bytes and fails on any byte past it.
type limitWriter struct {
	buf   bytes.Buffer
	limit int64
	over  bool
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > w.limit {
		w.over = true
		return 0, ErrFileTooLarge
	}
	return w.buf.Write(p)
}
