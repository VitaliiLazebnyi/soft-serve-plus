package cmd

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

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
