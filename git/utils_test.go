package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

// gitRun runs git in dir and returns its trimmed output.
func gitRun(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s failed: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// setupTreeRepo commits the given entries, each a path mapped to its git
// mode and content, straight into the index, so symlinks and submodules can
// be made on any OS. A submodule's content is the commit it points to.
func setupTreeRepo(t *testing.T, entries map[string][2]string) (*Repository, *Reference) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", dir)
	for p, e := range entries {
		mode, content := e[0], e[1]
		sha := content
		if mode != "160000" {
			sha = gitRun(t, dir, content, "hash-object", "-w", "--stdin")
		}
		gitRun(t, dir, "", "update-index", "--add", "--cacheinfo", mode+","+sha+","+p)
	}
	gitRun(t, dir, "", "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init")

	repo, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	return repo, ref
}

const someCommit = "1111111111111111111111111111111111111111"

func TestLatestFile(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{
		"docs/real.md": {"100644", "real"},
		// A symlink out of its own directory.
		".github/README": {"120000", "../docs/real.md"},
		// A chain of symlinks.
		"chain/README": {"120000", "next"},
		"chain/next":   {"120000", "../docs/real.md"},
		// A matching submodule comes before the file.
		"mods/README":    {"160000", someCommit},
		"mods/readme.md": {"100644", "mods"},
		// Symlinks that lead nowhere, out of the repository, to an
		// absolute path, to a directory, or in a loop are skipped.
		"broken/README":  {"120000", "missing"},
		"broken/Readme":  {"120000", "../../outside"},
		"broken/rEadme":  {"120000", "/etc/passwd"},
		"broken/reAdme":  {"120000", "../docs"},
		"broken/reaDme":  {"120000", "reaDme"},
		"broken/readme9": {"100644", "broken"},
	})

	for _, tc := range []struct {
		dir, content, path string
	}{
		{".github", "real", "docs/real.md"},
		{"chain", "real", "docs/real.md"},
		{"mods", "mods", "mods/readme.md"},
		{"broken", "broken", "broken/readme9"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			content, path, err := LatestFile(repo, ref, tc.dir+"/[rR][eE][aA][dD][mM][eE]*", 0)
			if err != nil {
				t.Fatalf("LatestFile: %v", err)
			}
			if strings.TrimSpace(content) != tc.content || path != tc.path {
				t.Errorf("LatestFile = %q, %q; want %q, %q", content, path, tc.content, tc.path)
			}
		})
	}
}

func TestTreeEntryModeAndPath(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{
		"dir/file": {"100644", "file"},
		"exec":     {"100755", "exec"},
		"link":     {"120000", "dir/file"},
		"sub":      {"160000", someCommit},
	})
	tree, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	for _, tc := range []struct {
		path    string
		mode    fs.FileMode
		regular bool
	}{
		{"dir", fs.ModeDir | fs.ModePerm, false},
		{"dir/file", 0o644, true},
		{"exec", 0o755, true},
		{"link", fs.ModeSymlink | fs.ModePerm, false},
		{"sub", fs.ModeIrregular, false},
	} {
		te, err := tree.TreeEntry(tc.path)
		if err != nil {
			t.Fatalf("TreeEntry(%q): %v", tc.path, err)
		}
		if got := te.Mode(); got != tc.mode || got.IsRegular() != tc.regular {
			t.Errorf("%s: Mode() = %v, want %v", tc.path, got, tc.mode)
		}
		if got := te.File().Path(); got != tc.path {
			t.Errorf("%s: path = %q", tc.path, got)
		}
	}
}

// TestLatestFilePatterns pins which file names a LatestFile pattern
// matches, for the README pattern the server uses and a few others. The
// syntax is path.Match's: negated classes ([^x], not gobwas's [!x]),
// alternatives ({a,b}) and ** are not part of it, and no caller uses them.
func TestLatestFilePatterns(t *testing.T) {
	const readme = "[rR][eE][aA][dD][mM][eE]*"
	cases := []struct {
		pattern, name string
		match         bool
	}{
		{readme, "README", true},
		{readme, "readme", true},
		{readme, "ReadMe.md", true},
		{readme, "README.md.bak", true},
		{readme, "readme-first.txt", true},
		{readme, "xREADME", false},
		{readme, "READM", false},
		{readme, "RE ADME", false},
		{readme, ".README", false},
		{"README*", "README-old", true},
		{"README*", "readme", false},
		{"README", "README", true},
		{"README", "README.md", false},
		{"*.md", "notes.md", true},
		{"*.md", "notes.txt", false},
		{"R?ADME", "README", true},
		{"R?ADME", "RAADME", true},
		{"R?ADME", "RADME", false},
		{"[a-z]*", "readme", true},
		{"[a-z]*", "README", false},
	}
	if runtime.GOOS != "windows" {
		// Git for Windows refuses "*" in paths, and reads "\" as a
		// separator.
		cases = append(cases,
			struct {
				pattern, name string
				match         bool
			}{`R\*`, "R*", true},
			struct {
				pattern, name string
				match         bool
			}{`R\*`, "RX", false},
		)
	}
	entries := map[string][2]string{}
	for i, tc := range cases {
		entries[fmt.Sprintf("c%02d/%s", i, tc.name)] = [2]string{"100644", "text"}
	}
	repo, ref := setupTreeRepo(t, entries)

	for i, tc := range cases {
		dir := fmt.Sprintf("c%02d", i)
		_, fp, err := LatestFile(repo, ref, dir+"/"+tc.pattern, 0)
		switch {
		case tc.match && (err != nil || fp != dir+"/"+tc.name):
			t.Errorf("%q against %q: got %q, %v; want a match", tc.pattern, tc.name, fp, err)
		case !tc.match && !errors.Is(err, ErrFileNotFound):
			t.Errorf("%q against %q: got %q, %v; want no match", tc.pattern, tc.name, fp, err)
		}
	}
}

// A malformed pattern is an error, not a panic.
func TestLatestFileBadPattern(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{"README": {"100644", "x"}})
	if _, _, err := LatestFile(repo, ref, "[README", 0); err == nil {
		t.Fatal("LatestFile with a malformed pattern succeeded")
	}
}

// N:git: LatestFile piped a matching file twice, once to sniff it and once
// to read it. It reads it once.
func TestLatestFileReadsOnce(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{"README.md": {"100644", "# hello\n"}})
	calls := gittest.CountGit(t)
	if _, _, err := LatestFile(repo, ref, "README*", 0); err != nil {
		t.Fatalf("LatestFile: %v", err)
	}
	reads := 0
	for _, c := range calls() {
		if c == "show "+gitRun(t, repo.Path, "", "rev-parse", "HEAD:README.md") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("LatestFile read the file %d times, want once", reads)
	}
}

// A binary match is skipped after its first bytes, with or without a size
// limit, rather than read in full; one over the limit is skipped as binary,
// not reported as too large.
func TestLatestFileSniffsBinary(t *testing.T) {
	const bigSize = 16 << 20
	bin := append([]byte{0}, bytes.Repeat([]byte("b"), bigSize)...)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"a/README":    bin,
		"b/README":    bin,
		"b/readme.md": []byte("text\n"),
	})
	for _, limit := range []int64{0, 1 << 20} {
		var err error
		n := allocated(func() { _, _, err = LatestFile(repo, ref, "a/README*", limit) })
		if !errors.Is(err, ErrFileNotFound) {
			t.Errorf("limit %d: err = %v, want ErrFileNotFound", limit, err)
		}
		if n > 1<<20 {
			t.Errorf("limit %d: skipping a %d MiB binary file allocated %d KiB", limit, bigSize>>20, n>>10)
		}
		content, fp, err := LatestFile(repo, ref, "b/[rR][eE][aA][dD][mM][eE]*", limit)
		if err != nil || fp != "b/readme.md" || content != "text\n" {
			t.Errorf("limit %d: LatestFile = %q, %q, %v; want b/readme.md", limit, content, fp, err)
		}
	}
}

func TestTextWriter(t *testing.T) {
	t.Parallel()
	w := &textWriter{limit: 10}
	if _, err := w.Write([]byte("0123456789")); err != nil || w.over || w.binary {
		t.Fatalf("10 bytes at limit 10: %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrFileTooLarge) || !w.over {
		t.Fatalf("past the limit: %v", err)
	}

	w = &textWriter{}
	if _, err := w.Write(bytes.Repeat([]byte("a"), sniffLen-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{0}); !errors.Is(err, errBinary) || !w.binary {
		t.Fatalf("NUL at byte %d: %v", sniffLen-1, err)
	}
	w = &textWriter{}
	if _, err := w.Write(append(bytes.Repeat([]byte("a"), sniffLen), 0)); err != nil || w.binary {
		t.Fatalf("NUL past the sniffed bytes: %v", err)
	}
	// Exactly sniffLen bytes, then a NUL in a write of its own.
	w = &textWriter{}
	if _, err := w.Write(bytes.Repeat([]byte("a"), sniffLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{0}); err != nil || w.binary {
		t.Fatalf("NUL in its own write past the sniffed bytes: %v", err)
	}
}
