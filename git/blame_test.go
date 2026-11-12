package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// setupBlameRepo creates a repository whose file f.txt has lines from three
// commits, and returns it with its HEAD.
func setupBlameRepo(t *testing.T) (*Repository, *Reference) {
	t.Helper()
	repo, _ := setupTestRepo(t)
	content := ""
	for i, add := range []string{"one\ntwo\n", "three\n", "four\nfive\n"} {
		content += add
		if err := os.WriteFile(filepath.Join(repo.Path, "f.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"add", "f.txt"},
			{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "blame " + strings.Repeat("x", i+1)},
		} {
			cmd := exec.CommandContext(context.Background(), "git", args...)
			cmd.Dir = repo.Path
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
			}
		}
	}
	repo, err := Open(repo.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	head, err := repo.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	return repo, head
}

func TestBlameFileMatchesGitModule(t *testing.T) {
	repo, head := setupBlameRepo(t)

	got, err := repo.WithContext(context.Background()).BlameFile(head.ID, "f.txt")
	if err != nil {
		t.Fatalf("BlameFile: %v", err)
	}
	want, err := repo.Repository.BlameFile(head.ID, "f.txt")
	if err != nil {
		t.Fatalf("git-module BlameFile: %v", err)
	}
	ids := make(map[string]bool)
	for i := 1; ; i++ {
		g, w := got.Line(i), want.Line(i)
		if g == nil || w == nil {
			if g != w {
				t.Fatalf("line %d: got %v, git-module %v", i, g, w)
			}
			if i != 6 {
				t.Fatalf("blame has %d lines; want 5", i-1)
			}
			break
		}
		if g.ID.String() != w.ID.String() || g.Message != w.Message || g.Author.Email != w.Author.Email {
			t.Fatalf("line %d = %s %q; git-module %s %q", i, g.ID, g.Message, w.ID, w.Message)
		}
		ids[g.ID.String()] = true
	}
	if len(ids) != 3 {
		t.Fatalf("blame names %d commits; want 3", len(ids))
	}
	if got.Line(0) != nil || (*Blame)(nil).Line(1) != nil {
		t.Fatal("Line out of range returned a commit")
	}
}

// cancelAfterCtx is a context whose Err reports cancellation once it has
// been asked n times. Its Done channel is nil, so git commands run under it
// are never killed: only code that checks Err stops.
type cancelAfterCtx struct {
	context.Context
	n     int32
	calls atomic.Int32
}

func (c *cancelAfterCtx) Done() <-chan struct{} { return nil }

func (c *cancelAfterCtx) Err() error {
	if c.calls.Add(1) > c.n {
		return context.Canceled
	}
	return nil
}

// TestBlameFileStopsResolvingCommitsOnCancel checks that a context cancelled
// after `git blame` has run stops the per-line commit reads.
func TestBlameFileStopsResolvingCommitsOnCancel(t *testing.T) {
	repo, head := setupBlameRepo(t)

	ctx := &cancelAfterCtx{Context: context.Background(), n: 2}
	b, err := repo.WithContext(ctx).BlameFile(head.ID, "f.txt")
	if err == nil {
		t.Fatalf("BlameFile with a context cancelled mid-way returned %v; want an error", b)
	}
	if err != context.Canceled { //nolint:errorlint
		t.Fatalf("BlameFile error = %v; want %v", err, context.Canceled)
	}
	if n := ctx.calls.Load(); n != 3 {
		t.Fatalf("context checked %d times; want 3 (stop at the third line)", n)
	}
}
