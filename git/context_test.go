package git

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
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

// Open runs git under its context, so a done context fails it.
func TestOpenUnderCancelledContext(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, repo.Path); err == nil {
		t.Fatal("Open under a cancelled context succeeded; want an error")
	}
	if _, err := OpenLocal(ctx, repo.Path); err == nil {
		t.Fatal("OpenLocal under a cancelled context succeeded; want an error")
	}
	if _, err := Init(ctx, filepath.Join(t.TempDir(), "new"), true); err == nil {
		t.Fatal("Init under a cancelled context succeeded; want an error")
	}
}

// An entry listed under one context reads its contents through a
// repository opened under another.
func TestEntryFileReadsUnderRepositoryContext(t *testing.T) {
	t.Parallel()
	repo, ref := setupTestRepo(t)
	listed, cancel := context.WithCancel(t.Context())
	lr, err := Open(listed, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := lr.TreePath(ref, "dot_config")
	if err != nil {
		t.Fatal(err)
	}
	ents, err := tree.Entries()
	if err != nil || len(ents) != 1 {
		t.Fatalf("entries = %v, %v; want one", ents, err)
	}
	cancel()
	if _, err := ents[0].File().Contents(); err == nil {
		t.Fatal("read an entry under a cancelled context")
	}
	c, err := repo.EntryFile(ents[0]).Contents()
	if err != nil || string(c) != "test content" {
		t.Fatalf("contents through a live repository = %q, %v", c, err)
	}
	if _, err := ents[0].File().Contents(); err == nil {
		t.Fatal("EntryFile changed the entry's own repository")
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

	cases := map[string]func(r *Repository) error{
		"HEAD":         func(r *Repository) error { _, err := r.HEAD(); return err },
		"References":   func(r *Repository) error { _, err := r.References(); return err },
		"Branches":     func(r *Repository) error { _, err := r.Branches(); return err },
		"RefInfos":     func(r *Repository) error { _, err := r.RefInfos(RefsHeads); return err },
		"Tree":         func(r *Repository) error { _, err := r.Tree(head); return err },
		"CountCommits": func(r *Repository) error { _, err := r.CountCommits(head); return err },
		"CommitsByPage": func(r *Repository) error {
			_, err := r.CommitsByPage(head, 1, 10)
			return err
		},
		"CatFileCommit":   func(r *Repository) error { _, err := r.CatFileCommit(head.ID()); return err },
		"Diff":            func(r *Repository) error { _, err := r.Diff(&Commit{ID: gitm.MustIDFromString(head.ID())}); return err },
		"BlameFile":       func(r *Repository) error { _, err := r.BlameFile(head.ID(), "dot_config/bat"); return err },
		"CheckAttributes": func(r *Repository) error { _, err := r.CheckAttributes(head, "dot_config/bat"); return err },
		"SymbolicRef":     func(r *Repository) error { _, err := r.SymbolicRef("HEAD", ""); return err },
		"Contents": func(r *Repository) error {
			e := *entry
			e.repo = r
			_, err := e.Contents()
			return err
		},
		"Entries": func(r *Repository) error {
			tr := *tree
			tr.repo = r
			_, err := tr.Entries()
			return err
		},
		"TreeEntry": func(r *Repository) error {
			tr := *tree
			tr.repo = r
			_, err := tr.TreeEntry("bat")
			return err
		},
	}
	for name, fn := range cases {
		// Open the repository afresh for each case: git-module caches trees
		// and commits per repository, and a cache hit runs no git command.
		ctx, cancel := context.WithCancel(t.Context())
		fresh, err := Open(ctx, live.Path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		cancel()
		if err := fn(fresh); err == nil {
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
	r := repo

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
		want, err := r.gm.CommitsByPage(head.Name().String(), tc.page, tc.size)
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
