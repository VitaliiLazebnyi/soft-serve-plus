package git

import (
	"errors"
	"io/fs"
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
