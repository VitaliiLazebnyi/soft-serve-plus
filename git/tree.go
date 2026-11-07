package git

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	gopath "path"
	"sort"

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

// TreeEntry returns the TreeEntry for the file path.
func (t *Tree) TreeEntry(path string) (*TreeEntry, error) {
	entry, err := t.Tree.TreeEntry(path, t.lsTreeOpts())
	if err != nil {
		return nil, err
	}
	return &TreeEntry{
		TreeEntry: entry,
		path:      gopath.Join(t.Path, entry.Name()),
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

// IsBinary returns true if the file is binary.
func (f *File) IsBinary() (bool, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := f.Pipeline(stdout, stderr)
	if err != nil {
		return false, err
	}
	r := bufio.NewReader(stdout)
	return IsBinary(r)
}

// Mode returns the mode of the file in fs.FileMode format.
func (e *TreeEntry) Mode() fs.FileMode {
	m := e.Blob().Mode()
	switch m {
	case git.EntryTree:
		return fs.ModeDir | fs.ModePerm
	default:
		return fs.FileMode(m) //nolint:gosec
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
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := f.Pipeline(stdout, stderr); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w - %s", err, stderr.String())
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
