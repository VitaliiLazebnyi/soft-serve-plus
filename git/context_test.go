package git

import (
	"context"
	"fmt"
	"os/exec"
	"testing"

	gitm "github.com/aymanbagabas/git-module"
)

// commitN adds n empty commits to the repository at path.
func commitN(t *testing.T, path string, n int) {
	t.Helper()
	for i := range n {
		cmd := exec.CommandContext(context.Background(), "git",
			"-c", "user.email=test@example.com", "-c", "user.name=Test",
			"commit", "--allow-empty", "-q", "-m", fmt.Sprintf("commit %d", i))
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit failed: %v\n%s", err, out)
		}
	}
}

func TestWithContextCopies(t *testing.T) {
	repo, _ := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rc := repo.WithContext(ctx)
	if rc == repo {
		t.Fatal("WithContext returned the same repository")
	}
	if rc.Context() != ctx {
		t.Fatal("WithContext did not set the context")
	}
	if repo.Context() != context.Background() {
		t.Fatal("WithContext changed the original repository's context")
	}
	if rc.Path != repo.Path || rc.IsBare != repo.IsBare || rc.Repository != repo.Repository {
		t.Fatal("WithContext did not keep the repository")
	}
}

// TestCancelledContextStopsCommands checks that every helper the TUI uses
// runs its git commands under the repository's context.
func TestCancelledContextStopsCommands(t *testing.T) {
	live, head := setupTestRepo(t)

	// Read the tree and file with a live context first, then swap in a
	// cancelled one, so the test reaches the commands that read contents.
	tree, err := live.TreePath(head, "dot_config")
	if err != nil {
		t.Fatalf("TreePath: %v", err)
	}
	entry, err := tree.TreeEntry("bat")
	if err != nil {
		t.Fatalf("TreeEntry: %v", err)
	}
	if got, err := entry.Contents(); err != nil || string(got) != "test content" {
		t.Fatalf("Contents with a live context = %q, %v; want %q", got, err, "test content")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := map[string]func(r *Repository) error{
		"HEAD":         func(r *Repository) error { _, err := r.HEAD(); return err },
		"References":   func(r *Repository) error { _, err := r.References(); return err },
		"Branches":     func(r *Repository) error { _, err := r.Branches(); return err },
		"Tree":         func(r *Repository) error { _, err := r.Tree(head); return err },
		"CountCommits": func(r *Repository) error { _, err := r.CountCommits(head); return err },
		"CommitsByPage": func(r *Repository) error {
			_, err := r.CommitsByPage(head, 1, 10)
			return err
		},
		"CatFileCommit":   func(r *Repository) error { _, err := r.CatFileCommit(head.ID); return err },
		"Diff":            func(r *Repository) error { _, err := r.Diff(&Commit{ID: gitm.MustIDFromString(head.ID)}); return err },
		"BlameFile":       func(r *Repository) error { _, err := r.BlameFile(head.ID, "dot_config/bat"); return err },
		"CheckAttributes": func(r *Repository) error { _, err := r.CheckAttributes(head, "dot_config/bat"); return err },
		"SymbolicRef":     func(r *Repository) error { _, err := r.SymbolicRef("HEAD", ""); return err },
		"Contents": func(r *Repository) error {
			e := *entry
			e.repo = entry.repo.WithContext(ctx)
			_, err := e.Contents()
			return err
		},
		"IsBinary": func(r *Repository) error {
			e := *entry
			e.repo = entry.repo.WithContext(ctx)
			_, err := e.File().IsBinary()
			return err
		},
	}
	for name, fn := range cases {
		// Open the repository afresh for each case: git-module caches trees
		// and commits per repository, and a cache hit runs no git command.
		fresh, err := Open(live.Path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := fn(fresh.WithContext(ctx)); err == nil {
			t.Errorf("%s with a cancelled context succeeded; want an error", name)
		}
	}
}

func TestCommitsByPage(t *testing.T) {
	repo, _ := setupTestRepo(t)
	commitN(t, repo.Path, 4) // five commits in all

	head, err := repo.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	r := repo.WithContext(context.Background())

	for _, tc := range []struct{ page, size, want int }{
		{1, 2, 2},
		{2, 2, 2},
		{3, 2, 1},
		{4, 2, 0},
		{1, 10, 5},
	} {
		got, err := r.CommitsByPage(head, tc.page, tc.size)
		if err != nil {
			t.Fatalf("CommitsByPage(%d, %d): %v", tc.page, tc.size, err)
		}
		want, err := r.Repository.CommitsByPage(head.Name().String(), tc.page, tc.size)
		if err != nil {
			t.Fatalf("git-module CommitsByPage(%d, %d): %v", tc.page, tc.size, err)
		}
		if len(got) != tc.want || len(want) != tc.want {
			t.Fatalf("CommitsByPage(%d, %d) returned %d commits (git-module %d); want %d",
				tc.page, tc.size, len(got), len(want), tc.want)
		}
		for i := range got {
			if got[i].ID.String() != want[i].ID.String() || got[i].Message != want[i].Message {
				t.Fatalf("CommitsByPage(%d, %d)[%d] = %s %q; want %s %q", tc.page, tc.size, i,
					got[i].ID, got[i].Message, want[i].ID, want[i].Message)
			}
		}
	}
}
