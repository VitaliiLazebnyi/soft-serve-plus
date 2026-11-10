package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pinQuotePath sets core.quotePath=true, git's default, for every git process
// the test starts, including the ls-tree that git-module runs, so names are
// C-quoted even on a machine whose config sets core.quotePath=false. It uses
// t.Setenv, so the calling test must not be parallel.
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

func TestFileIsBinary(t *testing.T) {
	const bigSize = 16 << 20
	big := bytes.Repeat([]byte("0123456789abcde\n"), bigSize/16)
	lateNUL := append(bytes.Repeat([]byte("a"), sniffLen), 0)
	bin := append([]byte{0}, bytes.Repeat([]byte("b"), bigSize)...)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"big.txt":   big,
		"late.txt":  lateNUL,
		"bin.dat":   bin,
		"small.txt": []byte("hello\n"),
		"empty.txt": {},
	})

	for name, want := range map[string]bool{
		"big.txt":   false,
		"late.txt":  false, // the NUL is past the sniffed prefix
		"bin.dat":   true,
		"small.txt": false,
		"empty.txt": false,
	} {
		got, err := treeFile(t, repo, ref, name).IsBinary()
		if err != nil {
			t.Fatalf("%s: IsBinary: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: IsBinary = %v, want %v", name, got, want)
		}
	}

	// Sniffing a large file must not buffer the whole blob.
	f := treeFile(t, repo, ref, "big.txt")
	if n := allocated(func() { _, _ = f.IsBinary() }); n > bigSize/4 {
		t.Errorf("IsBinary allocated %d bytes for a %d byte file", n, bigSize)
	}
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
