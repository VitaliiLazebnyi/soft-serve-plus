package cmd

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"testing"

	sgit "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// R10: `repo tree` on a directory with more than git.MaxTreeEntries entries
// lists the first ones and says on stderr that the listing is cut. Paths
// past the cap can still be read.
func TestRepoTreeTruncated(t *testing.T) {
	ctx, git := diffRepo(t)
	blob := git("x", "hash-object", "-w", "--stdin")
	var in strings.Builder
	for i := range sgit.MaxTreeEntries + 1 {
		fmt.Fprintf(&in, "100644 blob %s\tf%06d\n", blob, i)
	}
	big := git(in.String(), "mktree")
	git("", "read-tree", "--prefix=big/", big)
	git("", "commit", "-q", "-m", "big")
	git("", "push", "-q", "origin", "main")

	stdout, stderr, err := runRepoOutput(t, ctx, "tree", "diffs", "big")
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if n := strings.Count(stdout, "\n"); n != sgit.MaxTreeEntries {
		t.Errorf("tree printed %d lines, want %d", n, sgit.MaxTreeEntries)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, strconv.Itoa(sgit.MaxTreeEntries)) {
		t.Errorf("tree stderr = %q, want a note that the listing is truncated", stderr)
	}

	last := fmt.Sprintf("big/f%06d", sgit.MaxTreeEntries)
	stdout, stderr, err = runRepoOutput(t, ctx, "tree", "diffs", "main", last)
	if err != nil || stdout != "-rw-r--r--\t1 B\t "+path.Base(last)+"\n" || stderr != "" {
		t.Errorf("tree of an entry past the cap = %q, %q, %v", stdout, stderr, err)
	}
	stdout, _, err = runRepoOutput(t, ctx, "blob", "diffs", last)
	if err != nil || stdout != "x\n" {
		t.Errorf("blob of an entry past the cap = %q, %v", stdout, err)
	}
}

// treeRepo pushes a commit holding every kind of tree entry to the
// repository "diffs" and returns a function that runs `repo tree diffs`
// with the given arguments.
func treeRepo(t *testing.T) func(args ...string) (string, string, error) {
	t.Helper()
	ctx, git := diffRepo(t)
	add := func(mode, content, path string) {
		sha := content
		if mode != "160000" {
			sha = git(content, "hash-object", "-w", "--stdin")
		}
		git("", "update-index", "--add", "--cacheinfo", mode+","+sha+","+path)
	}
	add("100644", "zz", "Zeta.txt")
	add("100644", "xxx", "alpha/x")
	add("100644", "alpha", "alpha.txt")
	add("100755", "#!/bin/sh\n", "beta.sh")
	add("100644", "", "empty")
	add("120000", "alpha.txt", "link")
	add("160000", "1111111111111111111111111111111111111111", "mod")
	git("", "commit", "-q", "-m", "tree")
	git("", "push", "-q", "origin", "main")
	return func(args ...string) (string, string, error) {
		return runRepoOutput(t, ctx, append([]string{"tree", "diffs"}, args...)...)
	}
}

// TestRepoTreeOutputPinned pins the `repo tree` listing: directories and
// submodules first, then files, each with its mode and size.
func TestRepoTreeOutputPinned(t *testing.T) {
	run := treeRepo(t)
	root := "drwxrwxrwx\t-\t alpha\n" +
		"?---------\t-\t mod\n" +
		"-rw-r--r--\t2 B\t Zeta.txt\n" +
		"-rw-r--r--\t5 B\t alpha.txt\n" +
		"-rwxr-xr-x\t10 B\t beta.sh\n" +
		"-rw-r--r--\t-\t empty\n" +
		"Lrwxrwxrwx\t9 B\t link\n"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, root},
		{[]string{"main", "/"}, root},
		{[]string{"alpha"}, "-rw-r--r--\t3 B\t x\n"},
		{[]string{"main", "alpha/x"}, "-rw-r--r--\t3 B\t x\n"},
		{[]string{"main", "link"}, "Lrwxrwxrwx\t9 B\t link\n"},
	} {
		got, stderr, err := run(tc.args...)
		if err != nil {
			t.Fatalf("tree %v: %v", tc.args, err)
		}
		if got != tc.want || stderr != "" {
			t.Errorf("tree %v =\n%q\nstderr %q, want\n%q", tc.args, got, stderr, tc.want)
		}
	}
	for _, p := range []string{"nope", "alpha/nope", "alpha.txt/x"} {
		if _, _, err := run("main", p); !errors.Is(err, proto.ErrFileNotFound) {
			t.Errorf("tree of %q: err = %v, want ErrFileNotFound", p, err)
		}
	}
}
