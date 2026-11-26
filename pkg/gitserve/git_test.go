package gitserve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
)

func TestPktline(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		err  error
		out  []byte
	}{
		{
			name: "empty",
			in:   []byte{},
			out:  []byte("0005\n0000"),
		},
		{
			name: "simple",
			in:   []byte("hello"),
			out:  []byte("000ahello\n0000"),
		},
		{
			name: "newline",
			in:   []byte("hello\n"),
			out:  []byte("000bhello\n\n0000"),
		},
		{
			name: "error",
			err:  fmt.Errorf("foobar"),
			out:  []byte("000fERR foobar\n0000"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if c.err == nil {
				if err := WritePktline(&out, string(c.in)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := WritePktlineErr(&out, c.err); err != nil {
					t.Fatal(err)
				}
			}

			if !bytes.Equal(out.Bytes(), c.out) {
				t.Errorf("expected %q, got %q", c.out, out.Bytes())
			}
		})
	}
}

// EnsureWithin takes a repository name relative to the repositories
// directory, as the SSH server and the git daemon pass it.
func TestEnsureWithinBad(t *testing.T) {
	tmp := t.TempDir()
	for _, f := range []string{
		"..",
		"../../../",
		"../repo.git",
		"foo/../../repo.git",
		// A sibling whose name starts with the repos directory's name.
		"../" + filepath.Base(tmp) + "2/x",
		"../" + filepath.Base(tmp) + "x",
	} {
		if err := EnsureWithin(tmp, f); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("EnsureWithin(%q, %q) => %v, want ErrInvalidRepo", tmp, f, err)
		}
	}
}

func TestEnsureWithinGood(t *testing.T) {
	tmp := t.TempDir()
	for _, f := range []string{
		"",
		".",
		"repo.git",
		"foo/repo.git",
		"foo/../repo.git",
		"..foo",
		"foo/../bar",
	} {
		if err := EnsureWithin(tmp, f); err != nil {
			t.Errorf("EnsureWithin(%q, %q) => %v, want nil error", tmp, f, err)
		}
	}
}

func TestEnsureDefaultBranchEmpty(t *testing.T) {
	tmp := t.TempDir()
	r, err := git.Init(t.Context(), tmp, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := EnsureDefaultBranch(context.TODO(), r.Path); err != nil {
		t.Errorf("EnsureDefaultBranch(%q) => %v, want nil error", tmp, err)
	}
}

// branchRepo creates a bare repository with one commit, the given branches
// pointing at it, and HEAD pointing at head, which need not exist.
func branchRepo(t *testing.T, head string, branches ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	gittest.Git(t, "", nil, "init", "-q", "--bare", dir)
	tree := gittest.Tree(t, dir, "f", "x")
	commit := gittest.Git(t, dir, nil, "commit-tree", "-m", "c", tree)
	var in strings.Builder
	for _, b := range branches {
		fmt.Fprintf(&in, "create refs/heads/%s %s\n", b, commit)
	}
	if in.Len() > 0 {
		gittest.Git(t, dir, []byte(in.String()), "update-ref", "--stdin")
	}
	gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", "refs/heads/"+head)
	return dir
}

// TestEnsureDefaultBranch pins which branch HEAD is pointed at when it
// names a branch that does not exist, and that a HEAD that resolves, or a
// repository without branches, is left alone.
func TestEnsureDefaultBranch(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		branches   []string
		want       string
	}{
		{"main preferred", "gone", []string{"a", "main", "z"}, "main"},
		{"master preferred", "gone", []string{"a", "master", "z"}, "master"},
		{"first in ref order", "gone", []string{"c", "b"}, "b"},
		{"valid head kept", "z", []string{"a", "main", "z"}, "z"},
		{"no branches", "gone", nil, "gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := branchRepo(t, tc.head, tc.branches...)
			if err := EnsureDefaultBranch(t.Context(), dir); err != nil {
				t.Fatalf("EnsureDefaultBranch: %v", err)
			}
			if got := gittest.Git(t, dir, nil, "symbolic-ref", "HEAD"); got != "refs/heads/"+tc.want {
				t.Errorf("HEAD = %s, want refs/heads/%s", got, tc.want)
			}
		})
	}
}

// N:git: EnsureDefaultBranch listed every branch after each push before it
// looked at HEAD. With a HEAD that resolves it lists none.
func TestEnsureDefaultBranchValidHeadListsNothing(t *testing.T) {
	branches := make([]string, 2000)
	for i := range branches {
		branches[i] = fmt.Sprintf("b%04d", i)
	}
	dir := branchRepo(t, "b1000", branches...)
	calls := gittest.CountGit(t)
	if err := EnsureDefaultBranch(t.Context(), dir); err != nil {
		t.Fatalf("EnsureDefaultBranch: %v", err)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "for-each-ref") || strings.HasPrefix(c, "show-ref --heads") {
			t.Errorf("EnsureDefaultBranch listed the branches (git %s) though HEAD resolves", c)
		}
	}
}
