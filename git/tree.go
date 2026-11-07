package git

import (
	"bufio"
	"bytes"
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
	// name is the file name, unquoted
	name string
	// path is the full path of the file
	path string
}

func newTreeEntry(dir string, e *git.TreeEntry) *TreeEntry {
	name := unquoteName(e.Name())
	return &TreeEntry{
		TreeEntry: e,
		name:      name,
		path:      gopath.Join(dir, name),
	}
}

// unquoteName returns the real name of a tree entry. git ls-tree C-quotes
// names that contain special characters (and, with core.quotePath, non-ASCII
// bytes), e.g. "caf\303\251.txt", and git-module strips the surrounding
// double quotes but keeps the escapes. A name git did not quote never
// contains a backslash, so only names with one need unquoting.
func unquoteName(name string) string {
	if !strings.Contains(name, `\`) {
		return name
	}
	if n, err := strconv.Unquote(`"` + name + `"`); err == nil {
		return n
	}
	return name
}

// Name returns the name of the entry, unquoted.
func (e *TreeEntry) Name() string {
	return e.name
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

// SubTree returns the sub-tree at the given path, relative to t. The path
// uses real (unquoted) names.
func (t *Tree) SubTree(path string) (*Tree, error) {
	path = gopath.Clean(path)
	if path == "." || path == "/" {
		return t, nil
	}
	cur := t
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		e, err := cur.entry(name)
		if err != nil {
			return nil, err
		}
		if !e.IsTree() {
			return nil, ErrRevisionNotExist
		}
		tree, err := t.Repository.Repository.LsTree(e.ID().String())
		if err != nil {
			return nil, err
		}
		cur = &Tree{
			Tree:       tree,
			Path:       e.path,
			Repository: t.Repository,
		}
	}
	return cur, nil
}

// entry returns the direct child of t with the given name.
func (t *Tree) entry(name string) (*TreeEntry, error) {
	entries, err := t.Entries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() == name {
			return e, nil
		}
	}
	return nil, ErrRevisionNotExist
}

// Entries returns the entries in the tree.
func (t *Tree) Entries() (Entries, error) {
	entries, err := t.Tree.Entries()
	if err != nil {
		return nil, err
	}
	ret := make(Entries, len(entries))
	for i, e := range entries {
		ret[i] = newTreeEntry(t.Path, e)
	}
	return ret, nil
}

// TreeEntry returns the TreeEntry for the file path, relative to t. The path
// uses real (unquoted) names.
func (t *Tree) TreeEntry(path string) (*TreeEntry, error) {
	path = gopath.Clean(path)
	if path == "." || path == "/" {
		entry, err := t.Tree.TreeEntry("")
		if err != nil {
			return nil, err
		}
		return &TreeEntry{TreeEntry: entry, path: t.Path}, nil
	}
	dir, name := gopath.Split(path)
	tree := t
	if dir != "" {
		var err error
		tree, err = t.SubTree(dir)
		if err != nil {
			return nil, err
		}
	}
	return tree.entry(name)
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
	return f.Blob.Bytes()
}
