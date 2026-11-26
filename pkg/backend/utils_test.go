package backend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// gitIn runs git in dir with stdin and returns its trimmed output.
func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// readmeRepo creates a repository whose main branch, its HEAD, holds files,
// path to content, at the root or one directory down.
func readmeRepo(t *testing.T, files map[string]string) proto.Repository {
	t.Helper()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	r, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := be.repoPath("repo")
	root := map[string]string{}
	sub := map[string]map[string]string{}
	for p, content := range files {
		blob := gitIn(t, dir, content, "hash-object", "-w", "--stdin")
		if d, name, ok := strings.Cut(p, "/"); ok {
			if sub[d] == nil {
				sub[d] = map[string]string{}
			}
			sub[d][name] = "100644 blob " + blob + "\t" + name
		} else {
			root[p] = "100644 blob " + blob + "\t" + p
		}
	}
	for d, entries := range sub {
		var lines []string
		for _, e := range entries {
			lines = append(lines, e)
		}
		root[d] = "040000 tree " + gitIn(t, dir, strings.Join(lines, "\n")+"\n", "mktree") + "\t" + d
	}
	var lines []string
	for _, e := range root {
		lines = append(lines, e)
	}
	tree := gitIn(t, dir, strings.Join(lines, "\n")+"\n", "mktree")
	commit := gitIn(t, dir, "", "commit-tree", tree, "-m", "x")
	gitIn(t, dir, "", "update-ref", "refs/heads/main", commit)
	gitIn(t, dir, "", "symbolic-ref", "HEAD", "refs/heads/main")
	return r
}

// A README too large to show is reported as such, not as missing; one in a
// later directory is still shown.
func TestReadmeTooLarge(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 100)

	r := readmeRepo(t, map[string]string{"README.md": big})
	_, _, err := Readme(t.Context(), r, nil, 10)
	if !errors.Is(err, git.ErrFileTooLarge) {
		t.Fatal("not true: errors.Is(err, git.ErrFileTooLarge)")
	}

	r = readmeRepo(t, map[string]string{"README.md": big, "docs/README.md": "small"})
	readme, path, err := Readme(t.Context(), r, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := path; got != "docs/README.md" {
		t.Fatalf("path = %v, want %v", got, "docs/README.md")
	}
	if got := readme; got != "small" {
		t.Fatalf("readme = %v, want %v", got, "small")
	}

	r = readmeRepo(t, map[string]string{"main.go": "package main"})
	_, _, err = Readme(t.Context(), r, nil, 10)
	if !errors.Is(err, git.ErrFileNotFound) {
		t.Fatal("not true: errors.Is(err, git.ErrFileNotFound)")
	}
}

// A README found only under docs/ is still found: tree paths use "/" on
// every system, so the glob is not built with filepath.Join, which would
// give it a backslash on Windows.
func TestReadmeInSubdirectory(t *testing.T) {
	t.Parallel()
	lt := newLFSTestWith(t, 0, func(work string, _ []lfs.Pointer) {
		if err := os.MkdirAll(filepath.Join(work, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "docs", "README.md"), []byte("# Docs\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	// The helper pushes branch main, but the bare repository's HEAD follows
	// git's init.defaultBranch, which may still be master. Point HEAD at main
	// so the lookup below does not depend on the host's git config.
	r, err := lt.repo.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SymbolicRef("HEAD", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}

	readme, path, err := Readme(t.Context(), lt.repo, nil, 0)
	if err != nil {
		t.Fatalf("Readme: %v", err)
	}
	if path != "docs/README.md" || readme != "# Docs\n" {
		t.Fatalf("Readme = %q, %q; want %q, %q", readme, path, "# Docs\n", "docs/README.md")
	}
}

// A refs/replace ref, which any writer can push and a --mirror import
// fetches, must not change what the server shows: upload-pack ignores it,
// so a clone gets the original objects, and the server's own views must
// show the same, not the replacement.
func TestServerReadsIgnoreReplaceRefs(t *testing.T) {
	t.Parallel()
	r := readmeRepo(t, map[string]string{"README.md": "real"})
	rr, err := r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dir := rr.Path
	head := gitIn(t, dir, "", "rev-parse", "HEAD")
	blob := gitIn(t, dir, "fake", "hash-object", "-w", "--stdin")
	tree := gitIn(t, dir, "100644 blob "+blob+"\tREADME.md\n", "mktree")
	fake := gitIn(t, dir, "", "commit-tree", tree, "-m", "fake")
	gitIn(t, dir, "", "update-ref", "refs/replace/"+head, fake)

	readme, _, err := Readme(t.Context(), r, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := readme; got != "real" {
		t.Fatalf("readme = %v, want %v", got, "real")
	}
}
