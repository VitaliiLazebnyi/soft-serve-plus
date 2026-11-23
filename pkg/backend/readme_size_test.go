package backend

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
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
// later directory is still shown. Readme used to return only the error of
// the last directory it looked in.
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
