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

// MaxTreeEntries is the most entries a directory listing returns; see
// Tree.Entries. A listing is for a person to read, in the TUI or from `repo
// tree`, and ten thousand lines are already more than anyone scrolls. It
// bounds the memory of one listing to a few MiB (an entry costs a few hundred
// bytes, and a name is at most maxTreeRecord bytes), where a tree with half a
// million entries used to cost hundreds of MiB.
const MaxTreeEntries = 10_000

// maxTreeRecord is the longest `ls-tree -l -z` record, in bytes, a listing
// accepts: the mode, type, ID and size, and a name or path as long as the
// usual PATH_MAX. A longer one fails the listing, so a hostile tree cannot
// make a single name cost unbounded memory.
const maxTreeRecord = 4096 + 128

// Tree is a tree object of a repository: a directory at a revision.
type Tree struct {
	// id is the tree object's full hex ID.
	id string
	// Path is the tree's path from the root tree, "" for the root.
	Path       string
	Repository *Repository
}

// TreeEntry is an entry of a tree: a file, a directory, a symlink or a
// submodule.
type TreeEntry struct {
	mode git.EntryMode
	typ  git.ObjectType
	// id is the object's full hex ID.
	id string
	// name is the file name, as stored in the tree.
	name string
	// path is the full path of the file.
	path string
	// size is the object's size from the listing, 0 for a tree or a
	// submodule.
	size int64
	// repo is the repository the entry was read from. Its context bounds
	// reading the entry's contents.
	repo *Repository
}

// Name returns the name of the entry.
func (e *TreeEntry) Name() string { return e.name }

// ID returns the ID of the entry's object.
func (e *TreeEntry) ID() *git.SHA1 { return git.MustIDFromString(e.id) }

// Type returns the type of the entry's object: blob, tree or commit.
func (e *TreeEntry) Type() git.ObjectType { return e.typ }

// IsTree reports whether the entry is a directory.
func (e *TreeEntry) IsTree() bool { return e.mode == git.EntryTree }

// IsBlob reports whether the entry is a regular, non-executable file.
func (e *TreeEntry) IsBlob() bool { return e.mode == git.EntryBlob }

// IsExec reports whether the entry is an executable file.
func (e *TreeEntry) IsExec() bool { return e.mode == git.EntryExec }

// IsSymlink reports whether the entry is a symbolic link.
func (e *TreeEntry) IsSymlink() bool { return e.mode == git.EntrySymlink }

// IsCommit reports whether the entry is a submodule.
func (e *TreeEntry) IsCommit() bool { return e.mode == git.EntryCommit }

// Size returns the size of the entry's object, or zero for a tree or a
// submodule. It comes from the listing the entry was read from.
func (e *TreeEntry) Size() int64 { return e.size }

// Entries is a list of tree entries.
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

// File is a file of a tree, read through its entry.
type File struct {
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

// ID returns the ID of the file's blob.
func (f *File) ID() *git.SHA1 {
	return f.Entry.ID()
}

// Size returns the size of the file's blob.
func (f *File) Size() int64 {
	return f.Entry.Size()
}

// cleanTreePath cleans a path relative to a tree. It returns "" for the
// tree itself and false for a path that leaves the tree.
func cleanTreePath(path string) (string, bool) {
	path = strings.TrimPrefix(gopath.Clean(path), "/")
	if path == "." {
		path = ""
	}
	if path == ".." || strings.HasPrefix(path, "../") {
		return "", false
	}
	return path, true
}

// SubTree returns the sub-tree at the given path, relative to t.
func (t *Tree) SubTree(path string) (*Tree, error) {
	e, err := t.TreeEntry(path)
	if err != nil {
		return nil, err
	}
	if !e.IsTree() {
		return nil, ErrRevisionNotExist
	}
	return &Tree{id: e.id, Path: e.path, Repository: t.Repository}, nil
}

// Entries returns the entries of the tree, in git's order, read by one git
// process that also reports their sizes. A tree with more than
// MaxTreeEntries entries gives the first MaxTreeEntries along with an error
// wrapping ErrTreeTruncated; git is stopped there, so a huge tree costs no
// more than that.
func (t *Tree) Entries() (Entries, error) {
	ents, truncated, err := t.lsTree("", MaxTreeEntries)
	if err != nil {
		return nil, err
	}
	if truncated {
		return ents, fmt.Errorf("%w: showing the first %d entries", ErrTreeTruncated, MaxTreeEntries)
	}
	return ents, nil
}

// TreeEntry returns the TreeEntry for the file path, relative to t. It asks
// git for that one path, so a lookup does not list the directories on the
// way.
func (t *Tree) TreeEntry(path string) (*TreeEntry, error) {
	path, ok := cleanTreePath(path)
	if !ok {
		return nil, ErrRevisionNotExist
	}
	if path == "" {
		return &TreeEntry{
			mode: git.EntryTree,
			typ:  git.ObjectTree,
			id:   t.id,
			path: t.Path,
			repo: t.Repository,
		}, nil
	}
	// Without -t, git prints the entry at the path alone, not the trees on
	// the way to it; a malformed tree can hold duplicates of it.
	ents, _, err := t.lsTree(path, 4)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.name == path {
			e.name = gopath.Base(path)
			return e, nil
		}
	}
	return nil, ErrRevisionNotExist
}

// lsTree streams `git ls-tree -l -z` for t, or for the one path in t when
// path is not empty, and keeps at most limit entries. It reports whether
// git had more; git is stopped as soon as it does.
func (t *Tree) lsTree(path string, limit int) (Entries, bool, error) {
	args := []string{"ls-tree", "-l", "-z", "--full-tree", "--end-of-options", t.id}
	if path != "" {
		args = append(args, "--", path)
	}
	// Literal pathspecs: a name such as ":(glob)x" or "*" is a name.
	cmd := NewCommandWithContext(t.Repository.Context(), args...).AddEnvs(
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_GLOB_PATHSPECS=0",
		"GIT_NOGLOB_PATHSPECS=0",
		"GIT_ICASE_PATHSPECS=0",
	)
	p := &lsTreeParser{repo: t.Repository, dir: t.Path, limit: limit}
	var stderr bytes.Buffer
	err := cmd.RunInDirPipeline(p, &stderr, t.Repository.Path)
	switch {
	case p.err != nil:
		return nil, false, p.err
	case p.truncated:
		// git is stopped by the failed write, so err does not matter.
		return p.ents, true, nil
	case err != nil:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, false, fmt.Errorf("ls-tree: %w - %s", err, msg)
		}
		return nil, false, fmt.Errorf("ls-tree: %w", err)
	case len(p.pending) > 0:
		return nil, false, fmt.Errorf("%w: ls-tree output ends mid-entry", ErrMalformedTree)
	}
	return p.ents, false, nil
}

// errListingFull stops git once a listing has all the entries it keeps.
var errListingFull = errors.New("listing full")

// lsTreeParser parses `git ls-tree -l -z` output as git writes it, one
// NUL-terminated record at a time:
//
//	<mode> SP <type> SP <id> SP+ <size> TAB <name> NUL
//
// where the size is right-aligned and "-" for a tree or a submodule. The
// name is raw: -z turns off quoting, so it can hold any byte but NUL.
type lsTreeParser struct {
	repo      *Repository
	dir       string
	limit     int
	ents      Entries
	pending   []byte
	truncated bool
	err       error
}

func (p *lsTreeParser) Write(b []byte) (int, error) {
	if p.err != nil || p.truncated {
		return 0, errListingFull
	}
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			if len(p.pending)+len(b) > maxTreeRecord {
				p.err = fmt.Errorf("%w: entry longer than %d bytes", ErrMalformedTree, maxTreeRecord)
				return 0, p.err
			}
			p.pending = append(p.pending, b...)
			break
		}
		rec := b[:i]
		if len(p.pending) > 0 {
			if len(p.pending)+i > maxTreeRecord {
				p.err = fmt.Errorf("%w: entry longer than %d bytes", ErrMalformedTree, maxTreeRecord)
				return 0, p.err
			}
			rec = append(p.pending, rec...)
			p.pending = p.pending[:0]
		} else if i > maxTreeRecord {
			p.err = fmt.Errorf("%w: entry longer than %d bytes", ErrMalformedTree, maxTreeRecord)
			return 0, p.err
		}
		b = b[i+1:]
		if len(p.ents) == p.limit {
			p.truncated = true
			return 0, errListingFull
		}
		e, err := parseLsTreeRecord(rec)
		if err != nil {
			p.err = err
			return 0, err
		}
		e.repo = p.repo
		e.path = gopath.Join(p.dir, e.name)
		p.ents = append(p.ents, e)
	}
	return n, nil
}

// lsTreeModes maps the modes ls-tree prints to an entry mode and the object
// type that goes with it. 100664 is a blob mode old versions of git wrote.
var lsTreeModes = map[string]struct {
	mode git.EntryMode
	typ  git.ObjectType
}{
	"100644": {git.EntryBlob, git.ObjectBlob},
	"100664": {git.EntryBlob, git.ObjectBlob},
	"100755": {git.EntryExec, git.ObjectBlob},
	"120000": {git.EntrySymlink, git.ObjectBlob},
	"160000": {git.EntryCommit, git.ObjectCommit},
	"040000": {git.EntryTree, git.ObjectTree},
}

// parseLsTreeRecord parses one `ls-tree -l -z` record, without its NUL.
func parseLsTreeRecord(rec []byte) (*TreeEntry, error) {
	bad := func(what string) (*TreeEntry, error) {
		return nil, fmt.Errorf("%w: ls-tree entry with %s", ErrMalformedTree, what)
	}
	meta, name, ok := bytes.Cut(rec, []byte{'\t'})
	if !ok || len(name) == 0 {
		return bad("no name")
	}
	mode, meta, _ := bytes.Cut(meta, []byte{' '})
	typ, meta, _ := bytes.Cut(meta, []byte{' '})
	id, size, _ := bytes.Cut(meta, []byte{' '})
	size = bytes.TrimLeft(size, " ")

	m, ok := lsTreeModes[string(mode)]
	if !ok {
		return bad(fmt.Sprintf("unknown mode %q", mode))
	}
	if string(typ) != string(m.typ) {
		return bad(fmt.Sprintf("type %q for mode %s", typ, mode))
	}
	if !isFullHexID(id) {
		return bad("a malformed object ID")
	}
	e := &TreeEntry{mode: m.mode, typ: m.typ, id: string(id), name: string(name)}
	if m.typ == git.ObjectBlob {
		n, err := strconv.ParseInt(string(size), 10, 64)
		if err != nil || n < 0 {
			return bad(fmt.Sprintf("size %q", size))
		}
		e.size = n
	} else if string(size) != "-" {
		return bad(fmt.Sprintf("size %q for a %s", size, typ))
	}
	return e, nil
}

// isFullHexID reports whether id is a full-length SHA-1 object ID in
// lower-case hex, as git prints it.
func isFullHexID(id []byte) bool {
	if len(id) != 40 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
// stderr. It runs under the context of the repository the file was read
// from.
func (f *File) Pipeline(stdout, stderr io.Writer) error {
	r := f.Entry.repo
	return git.NewCommandWithContext(r.Context(), "show", f.Entry.id).
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
	switch e.mode {
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
	return &File{Entry: e}
}

// Contents returns the contents of the file.
func (e *TreeEntry) Contents() ([]byte, error) {
	return e.File().Contents()
}

// Contents returns the contents of the file.
func (f *File) Contents() ([]byte, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	// Preallocate the buffer from the listed size, so that a big file is
	// not read through repeated doubling.
	if size := f.Size(); size > 0 {
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
	// Cap the read as well, in case the listed size is wrong.
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
