package git

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

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
	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return repo, id
}

// R10: listing a directory of 200,000 entries read and kept all of them,
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
	repo, err := Open(dir)
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
	repo, err := Open(dir)
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
	repo, err := Open(dir)
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
