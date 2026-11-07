package git

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupQuotedNamesRepo creates a repository whose file names git C-quotes in
// ls-tree output. The tree is built with plumbing so that names which are
// not valid on every file system (quotes, backslashes, tabs) still work.
func setupQuotedNamesRepo(t *testing.T, paths ...string) (*Repository, *Reference) {
	t.Helper()
	ctx := context.Background()
	repoPath := filepath.Join(t.TempDir(), "repo")

	run := func(stdin string, args ...string) string {
		t.Helper()
		// core.quotePath is pinned so the test sees git's default quoting
		// whatever the machine's config says.
		args = append([]string{"-c", "core.quotePath=true"}, args...)
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

	repo, err := Open(repoPath)
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
	repo, ref := setupQuotedNamesRepo(t, ".gitattributes", dir+"/"+unicode, special, "plain.txt")

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
	for _, want := range []string{dir, special, "plain.txt"} {
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
	fp := dir + "/" + unicode
	if got := ents[0].Name(); got != unicode {
		t.Errorf("Name() = %q, want %q", got, unicode)
	}
	if got := ents[0].File().Path(); got != fp {
		t.Errorf("Path() = %q, want %q", got, fp)
	}

	for _, p := range []string{fp, special} {
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
		if _, err := repo.BlameFile(ref.ID, te.File().Path()); err != nil {
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

func TestUnquoteName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain.txt":            "plain.txt",
		`caf\303\251.txt`:      "café.txt",
		`a\\b\"c\td.txt`:       "a\\b\"c\td.txt",
		"café.txt":             "café.txt", // core.quotePath=false
		`not\x-a-valid-escape`: `not\x-a-valid-escape`,
	}
	for in, want := range cases {
		if got := unquoteName(in); got != want {
			t.Errorf("unquoteName(%q) = %q, want %q", in, got, want)
		}
	}
}
