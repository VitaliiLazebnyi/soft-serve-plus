package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pinQuotePath sets core.quotePath=true, git's default, for every git process
// the test starts, so any output that quotes names quotes them, even on a
// machine whose config sets core.quotePath=false. It uses t.Setenv, so the
// calling test must not be parallel.
func pinQuotePath(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.quotePath")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
}

// setupQuotedNamesRepo creates a repository whose file names git C-quotes in
// ls-tree output. The tree is built with plumbing so that names which are
// not valid on every file system (quotes, backslashes, tabs) still work,
// except on Windows, where git refuses such paths (core.protectNTFS).
func setupQuotedNamesRepo(t *testing.T, paths ...string) (*Repository, *Reference) {
	t.Helper()
	pinQuotePath(t)
	ctx := context.Background()
	repoPath := filepath.Join(t.TempDir(), "repo")

	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	cmd := exec.CommandContext(ctx, "git", "init", repoPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}
	for _, p := range paths {
		content := "content of " + p + "\n"
		if p == ".gitattributes" {
			content = "*.bin binary\n"
		}
		sha := run(content, "hash-object", "-w", "--stdin")
		run("", "update-index", "--add", "--cacheinfo", "100644,"+sha+","+p)
	}
	run("", "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "init")

	repo, err := Open(t.Context(), repoPath)
	if err != nil {
		t.Fatalf("failed to open repository: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	return repo, ref
}

// Tree entries must carry the real file names, not git's C-quoted form
// (e.g. caf\303\251.txt), so that blame, attributes and lookups by path work.
func TestTreeQuotedNames(t *testing.T) {
	const (
		dir     = "dir é"
		unicode = "café.bin"
		special = "a\\b\"c\td.txt"
	)
	paths := []string{".gitattributes", dir + "/" + unicode, "plain.txt"}
	rootNames := []string{dir, "plain.txt"}
	fp := dir + "/" + unicode
	lookups := []string{fp}
	// Git for Windows rejects backslashes, double quotes and control
	// characters in paths and reads a backslash in a pathspec as a
	// directory separator, so the special-character name is left out there.
	if runtime.GOOS != "windows" {
		paths = append(paths, special)
		rootNames = append(rootNames, special)
		lookups = append(lookups, special)
	}
	repo, ref := setupQuotedNamesRepo(t, paths...)

	root, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree failed: %v", err)
	}
	ents, err := root.Entries()
	if err != nil {
		t.Fatalf("Entries failed: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	for _, want := range rootNames {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("root entries = %q, want %q among them", names, want)
		}
	}

	tree, err := repo.TreePath(ref, dir)
	if err != nil {
		t.Fatalf("TreePath(%q) failed: %v", dir, err)
	}
	ents, err = tree.Entries()
	if err != nil {
		t.Fatalf("Entries failed: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("got %d entries, want 1", len(ents))
	}
	if got := ents[0].Name(); got != unicode {
		t.Errorf("Name() = %q, want %q", got, unicode)
	}
	if got := ents[0].File().Path(); got != fp {
		t.Errorf("Path() = %q, want %q", got, fp)
	}

	for _, p := range lookups {
		te, err := root.TreeEntry(p)
		if err != nil {
			t.Fatalf("TreeEntry(%q) failed: %v", p, err)
		}
		if got := te.File().Path(); got != p {
			t.Errorf("TreeEntry(%q).Path() = %q", p, got)
		}
		bts, err := te.Contents()
		if err != nil {
			t.Fatalf("Contents(%q) failed: %v", p, err)
		}
		if want := "content of " + p + "\n"; string(bts) != want {
			t.Errorf("Contents(%q) = %q, want %q", p, bts, want)
		}
		if _, err := repo.BlameFile(ref.ID(), te.File().Path()); err != nil {
			t.Errorf("BlameFile(%q) failed: %v", p, err)
		}
	}

	attrs, err := repo.CheckAttributes(ref, fp)
	if err != nil {
		t.Fatalf("CheckAttributes failed: %v", err)
	}
	if !containsAttr(attrs, Attribute{Name: "binary", Value: "set"}) {
		t.Errorf("CheckAttributes(%q) = %v, want binary set", fp, attrs)
	}

	if _, err := root.TreeEntry(dir + "/missing"); !errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("TreeEntry(missing) error = %v, want ErrRevisionNotExist", err)
	}
	if _, err := root.SubTree("plain.txt"); !errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("SubTree(file) error = %v, want ErrRevisionNotExist", err)
	}
}

func containsAttr(attrs []Attribute, want Attribute) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

// newFilesRepo creates a temp git repo with one commit holding files and
// returns the opened repository and its HEAD reference.
func newFilesRepo(t *testing.T, files map[string][]byte) (*Repository, *Reference) {
	t.Helper()
	ctx := context.Background()

	repoPath := filepath.Join(t.TempDir(), "repo")
	for name, data := range files {
		p := filepath.Join(repoPath, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"init"},
		{"-c", "core.autocrlf=false", "add", "."},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	repo, err := Open(t.Context(), repoPath)
	if err != nil {
		t.Fatalf("failed to open repository: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	return repo, ref
}

func treeFile(t *testing.T, repo *Repository, ref *Reference, name string) *File {
	t.Helper()
	tree, err := repo.TreePath(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	te, err := tree.TreeEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	return te.File()
}

// allocated returns the bytes allocated while running fn.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestFileContentsLimit(t *testing.T) {
	const bigSize = 16 << 20
	big := bytes.Repeat([]byte("x"), bigSize)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"big.txt":   big,
		"small.txt": []byte("hello\n"),
	})

	small := treeFile(t, repo, ref, "small.txt")
	for _, limit := range []int64{0, -1, 6, 1 << 20} {
		got, err := small.ContentsLimit(limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if string(got) != "hello\n" {
			t.Errorf("limit %d: got %q", limit, got)
		}
	}
	if _, err := small.ContentsLimit(5); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("limit 5 on 6 bytes: err = %v, want ErrFileTooLarge", err)
	}

	f := treeFile(t, repo, ref, "big.txt")
	var err error
	n := allocated(func() { _, err = f.ContentsLimit(1 << 20) })
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if n > bigSize/4 {
		t.Errorf("refusing a %d byte file allocated %d bytes", bigSize, n)
	}
}

func TestLimitWriter(t *testing.T) {
	t.Parallel()
	w := &limitWriter{limit: 4}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("e")); !errors.Is(err, ErrFileTooLarge) || !w.over {
		t.Fatalf("err = %v, over = %v; want ErrFileTooLarge", err, w.over)
	}
	if w.buf.String() != "abcd" {
		t.Errorf("buffered %q", w.buf.String())
	}
}

func TestLatestFileSkipsBinaryAndTooLarge(t *testing.T) {
	repo, ref := newFilesRepo(t, map[string][]byte{
		"README":           {0x89, 'P', 'N', 'G', 0, 0}, // sorts first
		"README.md":        []byte("# hello\n"),
		"docs/README":      bytes.Repeat([]byte("y"), 100),
		"docs/README.md":   []byte("# docs\n"),
		"large/README.md":  bytes.Repeat([]byte("z"), 100),
		"binary/README.md": {0, 1, 2},
	})

	got, fp, err := LatestFile(repo, ref, "README*", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "README.md" || got != "# hello\n" {
		t.Errorf("got %q at %q, want the text README.md", got, fp)
	}

	// docs/README is over the limit; the next match fits.
	got, fp, err = LatestFile(repo, ref, "docs/README*", 50)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "docs/README.md" || got != "# docs\n" {
		t.Errorf("got %q at %q, want docs/README.md", got, fp)
	}

	if _, _, err := LatestFile(repo, ref, "large/README*", 50); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("only match too large: err = %v, want ErrFileTooLarge", err)
	}
	if _, _, err := LatestFile(repo, ref, "binary/README*", 0); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("only match binary: err = %v, want ErrFileNotFound", err)
	}
}

func TestWriteBlob(t *testing.T) {
	data := append(bytes.Repeat([]byte("line\n"), 1<<16), 0, 1, 2)
	repo, ref := newFilesRepo(t, map[string][]byte{"data.bin": data})
	f := treeFile(t, repo, ref, "data.bin")

	var out bytes.Buffer
	if err := repo.WriteBlob(t.Context(), f.ID(), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Errorf("streamed %d bytes, want the %d byte blob", out.Len(), len(data))
	}
}

// TestContentsPreallocates checks that File.Contents reads a big file into a
// buffer sized for it instead of growing one by doubling.
func TestContentsPreallocates(t *testing.T) {
	repo, _ := setupTestRepo(t)
	const size = 1<<20 + 123
	want := bytes.Repeat([]byte("abcdefghij"), size/10+1)[:size]
	if err := os.WriteFile(filepath.Join(repo.Path, "big"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "big"},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "big"},
	} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = repo.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
	}
	r := repo
	head, err := r.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	tree, err := r.Tree(head)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	entry, err := tree.TreeEntry("big")
	if err != nil {
		t.Fatalf("TreeEntry: %v", err)
	}
	got, err := entry.Contents()
	if err != nil {
		t.Fatalf("Contents: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Contents returned %d bytes that differ from the %d written", len(got), len(want))
	}
	// Doubling would leave a capacity of at least 1.5 MiB here.
	if c := cap(got); c > size+64*1024 {
		t.Fatalf("Contents buffer capacity = %d for a %d-byte file; want it preallocated", c, size)
	}
}

// TestTreeEntriesSizes checks that a listing carries the sizes of its
// entries, read with the listing.
func TestTreeEntriesSizes(t *testing.T) {
	repo, ref := setupTestRepo(t)

	tree, err := repo.TreePath(ref, "")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err := tree.Entries()
	if err != nil {
		t.Fatalf("Entries failed: %v", err)
	}
	if len(ents) != 1 || !ents[0].IsTree() || ents[0].Size() != 0 {
		t.Fatalf("expected one tree entry of size 0, got %d entries", len(ents))
	}

	sub, err := repo.TreePath(ref, "dot_config")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err = sub.Entries()
	if err != nil {
		t.Fatalf("Entries failed: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "bat" {
		t.Fatalf("expected the bat entry, got %d entries", len(ents))
	}
	if got, want := ents[0].Size(), int64(len("test content")); got != want {
		t.Fatalf("size = %d, want %d", got, want)
	}
}

// entryKind names the kind of a tree entry by its Is* methods.
func entryKind(e *TreeEntry) string {
	switch {
	case e.IsTree():
		return "tree"
	case e.IsCommit():
		return "commit"
	case e.IsSymlink():
		return "symlink"
	case e.IsExec():
		return "exec"
	case e.IsBlob():
		return "blob"
	}
	return "?"
}

// TestTreeListingPinned pins what a listing reports for every kind of entry:
// the display order after Sort, the git type, the kind, the mode, the size
// and the path. Trees and submodules sort first, then each group by name in
// byte order.
func TestTreeListingPinned(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{
		"Zeta.txt":  {"100644", "zz"},
		"alpha/x":   {"100644", "xxx"},
		"alpha.txt": {"100644", "alpha"},
		"beta.sh":   {"100755", "#!/bin/sh\n"},
		"Dir2/y":    {"100644", "y"},
		"empty":     {"100644", ""},
		"link":      {"120000", "alpha.txt"},
		"mod":       {"160000", someCommit},
	})
	type row struct {
		name, typ, kind string
		mode            fs.FileMode
		size            int64
	}
	want := []row{
		{"Dir2", "tree", "tree", fs.ModeDir | fs.ModePerm, 0},
		{"alpha", "tree", "tree", fs.ModeDir | fs.ModePerm, 0},
		{"mod", "commit", "commit", fs.ModeIrregular, 0},
		{"Zeta.txt", "blob", "blob", 0o644, 2},
		{"alpha.txt", "blob", "blob", 0o644, 5},
		{"beta.sh", "blob", "exec", 0o755, 10},
		{"empty", "blob", "blob", 0o644, 0},
		{"link", "blob", "symlink", fs.ModeSymlink | fs.ModePerm, 9},
	}

	tree, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	ents, err := tree.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	ents.Sort()
	if len(ents) != len(want) {
		t.Fatalf("got %d entries, want %d", len(ents), len(want))
	}
	for i, e := range ents {
		got := row{e.Name(), string(e.Type()), entryKind(e), e.Mode(), e.Size()}
		if got != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got, want[i])
		}
		if p := e.File().Path(); p != want[i].name {
			t.Errorf("entry %d path = %q, want %q", i, p, want[i].name)
		}
	}

	sub, err := repo.TreePath(ref, "alpha")
	if err != nil {
		t.Fatalf("TreePath: %v", err)
	}
	ents, err = sub.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "x" || ents[0].File().Path() != "alpha/x" || ents[0].Size() != 3 {
		t.Fatalf("alpha entries = %v, want x at alpha/x of 3 bytes", ents)
	}

	for _, tc := range []struct {
		path, kind string
		size       int64
	}{
		{"alpha/x", "blob", 3},
		{"link", "symlink", 9},
		{"beta.sh", "exec", 10},
		{"alpha", "tree", 0},
		{"mod", "commit", 0},
		{"/alpha//x/", "blob", 3},
	} {
		te, err := tree.TreeEntry(tc.path)
		if err != nil {
			t.Fatalf("TreeEntry(%q): %v", tc.path, err)
		}
		if entryKind(te) != tc.kind || te.Size() != tc.size {
			t.Errorf("TreeEntry(%q) = %s of %d bytes, want %s of %d", tc.path, entryKind(te), te.Size(), tc.kind, tc.size)
		}
	}
	te, err := tree.TreeEntry("alpha/x")
	if err != nil {
		t.Fatalf("TreeEntry: %v", err)
	}
	if te.Name() != "x" || te.File().Path() != "alpha/x" {
		t.Errorf("TreeEntry(alpha/x) = %q at %q", te.Name(), te.File().Path())
	}
	for _, p := range []string{".", "/", ""} {
		root, err := tree.TreeEntry(p)
		if err != nil {
			t.Fatalf("TreeEntry(%q): %v", p, err)
		}
		if !root.IsTree() || root.Type() != "tree" || root.Size() != 0 {
			t.Errorf("TreeEntry(%q) = %s %q of %d bytes, want the root tree", p, entryKind(root), root.Type(), root.Size())
		}
	}
	for _, p := range []string{"missing", "alpha/missing", "alpha.txt/x", "mod/x", "Alpha/x", "../alpha.txt", "alpha/../alpha.txt/x"} {
		if _, err := tree.TreeEntry(p); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("TreeEntry(%q) error = %v, want ErrRevisionNotExist", p, err)
		}
	}
	for _, p := range []string{"alpha.txt", "mod", "missing"} {
		if _, err := tree.SubTree(p); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("SubTree(%q) error = %v, want ErrRevisionNotExist", p, err)
		}
	}
}

// A README in a directory too large to list in full is still found among
// the entries listed.
func TestLatestFileInTruncatedTree(t *testing.T) {
	repo, id := bigTreeRepo(t, MaxTreeEntries+1, "README.md")
	commit := gitRun(t, repo.Path, "", "-c", "user.name=t", "-c", "user.email=t@example.com",
		"commit-tree", "-m", "big", id)
	gitRun(t, repo.Path, "", "update-ref", "refs/heads/main", commit)
	gitRun(t, repo.Path, "", "symbolic-ref", "HEAD", "refs/heads/main")

	content, fp, err := LatestFile(repo, nil, "[rR][eE][aA][dD][mM][eE]*", 0)
	if err != nil || fp != "README.md" || content != "x" {
		t.Fatalf("LatestFile = %q, %q, %v; want README.md", content, fp, err)
	}
}

// bigTreeRepo creates a repository holding a tree of n files, f000000 and
// on, and the extra files given, all holding "x". The tree is built with
// mktree so no work tree is needed. It returns the repository and the
// tree's ID.
func bigTreeRepo(t *testing.T, n int, extra ...string) (*Repository, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", "-q", "--bare", dir)
	blob := gitRun(t, dir, "x", "hash-object", "-w", "--stdin")
	var in strings.Builder
	for i := range n {
		fmt.Fprintf(&in, "100644 blob %s\tf%06d\n", blob, i)
	}
	for _, name := range extra {
		fmt.Fprintf(&in, "100644 blob %s\t%s\n", blob, name)
	}
	id := gitRun(t, dir, in.String(), "mktree")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return repo, id
}

// Listing a directory of 200,000 entries read and kept all of them,
// several times over (hundreds of MiB). The listing must stop after
// MaxTreeEntries, say it did, and stay within a few MiB; looking up one
// path must not list the directory at all.
func TestTreeEntriesBounded(t *testing.T) {
	repo, id := bigTreeRepo(t, 200_000)
	tree, err := repo.LsTree(id)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}

	var ents Entries
	n := allocated(func() { ents, err = tree.Entries() })
	if !errors.Is(err, ErrTreeTruncated) {
		t.Errorf("Entries error = %v, want ErrTreeTruncated", err)
	}
	if len(ents) != MaxTreeEntries {
		t.Fatalf("Entries returned %d entries, want %d", len(ents), MaxTreeEntries)
	}
	if first, last := ents[0].Name(), ents[len(ents)-1].Name(); first != "f000000" || last != "f009999" {
		t.Errorf("Entries = %s..%s, want the first entries in git's order", first, last)
	}
	if ents[0].Size() != 1 || !ents[0].IsBlob() {
		t.Errorf("first entry: size %d, blob %v", ents[0].Size(), ents[0].IsBlob())
	}
	t.Logf("listing of %d entries allocated %d KiB", len(ents), n>>10)
	const bound = 8 << 20
	if n > bound {
		t.Errorf("listing allocated %d MiB, want under %d MiB", n>>20, bound>>20)
	}

	// An entry past the cap is still found, without listing the tree.
	var te *TreeEntry
	n = allocated(func() { te, err = tree.TreeEntry("f199999") })
	if err != nil {
		t.Fatalf("TreeEntry past the cap: %v", err)
	}
	if te.Name() != "f199999" || te.Size() != 1 {
		t.Errorf("TreeEntry = %q of %d bytes", te.Name(), te.Size())
	}
	t.Logf("lookup allocated %d KiB", n>>10)
	if n > 1<<20 {
		t.Errorf("lookup allocated %d KiB, want under 1 MiB", n>>10)
	}
}

// A directory of exactly MaxTreeEntries entries is listed in full.
func TestTreeEntriesAtCap(t *testing.T) {
	repo, id := bigTreeRepo(t, MaxTreeEntries)
	tree, err := repo.LsTree(id)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	ents, err := tree.Entries()
	if err != nil || len(ents) != MaxTreeEntries {
		t.Fatalf("Entries = %d entries, %v; want %d and no error", len(ents), err, MaxTreeEntries)
	}
}

// Long names cannot multiply the cost of a listing: it stops once the names
// and paths kept pass maxTreeTextBytes, well before MaxTreeEntries.
func TestTreeEntriesNameBudget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", "-q", "--bare", dir)
	blob := gitRun(t, dir, "x", "hash-object", "-w", "--stdin")
	const nameLen, n = 4000, 1000
	var in strings.Builder
	for i := range n {
		fmt.Fprintf(&in, "100644 blob %s\t%04d%s\n", blob, i, strings.Repeat("n", nameLen-4))
	}
	id := gitRun(t, dir, in.String(), "mktree")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tree, err := repo.LsTree(id)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	var ents Entries
	alloc := allocated(func() { ents, err = tree.Entries() })
	if !errors.Is(err, ErrTreeTruncated) {
		t.Fatalf("Entries error = %v, want ErrTreeTruncated", err)
	}
	// Each entry counts its name and its path, the same here at the root.
	if want := maxTreeTextBytes / (2 * nameLen); len(ents) != want {
		t.Errorf("Entries kept %d entries, want %d", len(ents), want)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(len(ents))) {
		t.Errorf("error %q does not give the number of entries shown", err)
	}
	t.Logf("listing of %d long names allocated %d KiB", len(ents), alloc>>10)
	if alloc > 12<<20 {
		t.Errorf("listing allocated %d MiB, want under 12 MiB", alloc>>20)
	}
}

// The text budget counts each entry's path, not only its name: deep in a
// tree of long directory names, short names still cost a long path each.
func TestTreeEntriesPathBudget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", "-q", "--bare", dir)
	blob := gitRun(t, dir, "x", "hash-object", "-w", "--stdin")
	var in strings.Builder
	for i := range MaxTreeEntries {
		fmt.Fprintf(&in, "100644 blob %s\tf%04d\n", blob, i)
	}
	id := gitRun(t, dir, in.String(), "mktree")
	// 15 nested directories of 250-byte names: a path of about 3.8 KiB.
	const depth, dirLen = 15, 250
	var parts []string
	for i := range depth {
		name := fmt.Sprintf("%02d%s", i, strings.Repeat("d", dirLen-2))
		parts = append(parts, name)
	}
	for i := depth - 1; i >= 0; i-- {
		id = gitRun(t, dir, "040000 tree "+id+"\t"+parts[i]+"\n", "mktree")
	}
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	root, err := repo.LsTree(id)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	tree, err := root.SubTree(strings.Join(parts, "/"))
	if err != nil {
		t.Fatalf("SubTree: %v", err)
	}
	var ents Entries
	alloc := allocated(func() { ents, err = tree.Entries() })
	t.Logf("listing of %d entries under a %d-byte path allocated %d KiB", len(ents), len(tree.Path), alloc>>10)
	if !errors.Is(err, ErrTreeTruncated) {
		t.Errorf("Entries error = %v, want ErrTreeTruncated", err)
	}
	if alloc > 12<<20 {
		t.Errorf("listing allocated %d MiB, want under 12 MiB", alloc>>20)
	}
}

// A blob missing from the repository lists with size 0 rather than failing
// the listing: ls-tree -l prints its size as BAD.
func TestTreeEntriesMissingBlob(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", "-q", "--bare", dir)
	blob := gitRun(t, dir, "x", "hash-object", "-w", "--stdin")
	const missing = "0123456789abcdef0123456789abcdef01234567"
	id := gitRun(t, dir, "100644 blob "+missing+"\tgone\n100644 blob "+blob+"\there\n", "mktree", "--missing")
	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tree, err := repo.LsTree(id)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	ents, err := tree.Entries()
	if err != nil || len(ents) != 2 {
		t.Fatalf("Entries = %d entries, %v; want both", len(ents), err)
	}
	if ents[0].Name() != "gone" || ents[0].Size() != 0 || ents[1].Size() != 1 {
		t.Errorf("Entries = %q of %d bytes, %q of %d bytes", ents[0].Name(), ents[0].Size(), ents[1].Name(), ents[1].Size())
	}
	if te, err := tree.TreeEntry("gone"); err != nil || te.Size() != 0 {
		t.Errorf("TreeEntry(gone) = %v", err)
	}
}

func TestParseLsTreeRecord(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		rec  string
		ok   bool
		kind string
		size int64
		name string
	}{
		{"100644 blob " + id + "      12\tfile", true, "blob", 12, "file"},
		{"100644 blob " + id + "     BAD\tmissing", true, "blob", 0, "missing"},
		{"100664 blob " + id + "       0\told mode", false, "", 0, ""},
		{"040000 tree " + id + "     BAD\td", false, "", 0, ""},
		{"100755 blob " + id + " 1234567890\tx", true, "exec", 1234567890, "x"},
		{"120000 blob " + id + "       3\tl", true, "symlink", 3, "l"},
		{"040000 tree " + id + "       -\td", true, "tree", 0, "d"},
		{"160000 commit " + id + "       -\tm", true, "commit", 0, "m"},
		{"100644 blob " + id + "       1\ttab\tand\nnewline", true, "blob", 1, "tab\tand\nnewline"},
		{"100644 blob " + id + "       1\t", false, "", 0, ""},
		{"100644 blob " + id + "       1", false, "", 0, ""},
		{"100644 blob " + strings.ToUpper(id) + "       1\tf", false, "", 0, ""},
		{"100644 blob " + id[:39] + "       1\tf", false, "", 0, ""},
		{"100644 blob " + id + "0       1\tf", false, "", 0, ""},
		{"100644 blob g" + id[1:] + "       1\tf", false, "", 0, ""},
		{"100600 blob " + id + "       1\tf", false, "", 0, ""},
		{"100644 tree " + id + "       1\tf", false, "", 0, ""},
		{"040000 tree " + id + "       1\td", false, "", 0, ""},
		{"100644 blob " + id + "       -\tf", false, "", 0, ""},
		{"100644 blob " + id + "      -1\tf", false, "", 0, ""},
		{"", false, "", 0, ""},
	} {
		e, err := parseLsTreeRecord([]byte(tc.rec))
		if !tc.ok {
			if !errors.Is(err, ErrMalformedTree) {
				t.Errorf("%q: err = %v, want ErrMalformedTree", tc.rec, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.rec, err)
			continue
		}
		if entryKind(e) != tc.kind || e.Size() != tc.size || e.Name() != tc.name || e.ID().String() != id {
			t.Errorf("%q = %s of %d bytes named %q", tc.rec, entryKind(e), e.Size(), e.Name())
		}
	}
}

// The parser takes records split across writes at any byte, and refuses a
// record longer than maxTreeRecord.
func TestLsTreeParserWrites(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	out := "100644 blob " + id + "       5\ta\x00040000 tree " + id + "       -\tdir\x00"
	for step := 1; step <= len(out); step++ {
		p := &lsTreeParser{dir: "top", limit: 10}
		for i := 0; i < len(out); i += step {
			if _, err := p.Write([]byte(out[i:min(i+step, len(out))])); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		if p.err != nil || len(p.pending) != 0 || len(p.ents) != 2 ||
			p.ents[0].path != "top/a" || p.ents[1].path != "top/dir" || !p.ents[1].IsTree() {
			t.Fatalf("step %d: %d entries, err %v, pending %q", step, len(p.ents), p.err, p.pending)
		}
	}

	long := "100644 blob " + id + "       5\t" + strings.Repeat("n", maxTreeRecord)
	for _, chunks := range [][]string{{long + "\x00"}, {long[:100], long[100:] + "\x00"}, {long}} {
		p := &lsTreeParser{limit: 10}
		var err error
		for _, c := range chunks {
			if _, err = p.Write([]byte(c)); err != nil {
				break
			}
		}
		if !errors.Is(err, ErrMalformedTree) {
			t.Errorf("record of %d bytes in %d writes: err = %v, want ErrMalformedTree", len(long), len(chunks), err)
		}
	}
}

// Names that look like pathspec magic or globs are looked up literally.
func TestTreeEntryLiteralPath(t *testing.T) {
	entries := map[string][2]string{
		"a/b":    {"100644", "b"},
		"dir/xy": {"100644", "xy"},
	}
	want := map[string]string{"a/b": "b"}
	missing := []string{":(icase)A/B", ":(exclude)a", ":(glob)a/*"}
	// Git for Windows refuses colons, "*", "?" and brackets in paths.
	if runtime.GOOS != "windows" {
		for name, content := range map[string]string{
			":(glob)q":  "magic",
			":!exclude": "ex",
			"a/*":       "star",
			"[ab]":      "class",
			"dir/x?":    "q",
		} {
			entries[name] = [2]string{"100644", content}
			want[name] = content
		}
		missing = append(missing, "a/?", "dir/x*", "[a]")
	}
	repo, ref := setupTreeRepo(t, entries)
	tree, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	for p, want := range want {
		te, err := tree.TreeEntry(p)
		if err != nil {
			t.Errorf("TreeEntry(%q): %v", p, err)
			continue
		}
		if got, err := te.Contents(); err != nil || string(got) != want {
			t.Errorf("TreeEntry(%q) contents = %q, %v; want %q", p, got, err, want)
		}
	}
	for _, p := range missing {
		if _, err := tree.TreeEntry(p); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("TreeEntry(%q) error = %v, want ErrRevisionNotExist", p, err)
		}
	}
}
