package gitserve

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

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
func TestEnsureDefaultBranchValidHeadListsNothing(t *testing.T) { //nolint: paralleltest
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
