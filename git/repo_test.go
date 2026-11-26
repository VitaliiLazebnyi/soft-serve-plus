package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git/gittest"
)

// setupTestRepo creates a temp git repo containing dot_config/bat and returns
// the opened repository and its HEAD reference.
func setupTestRepo(t *testing.T) (*Repository, *Reference) {
	t.Helper()
	ctx := context.Background()

	repoPath := filepath.Join(t.TempDir(), "test-repo")
	nestedDir := filepath.Join(repoPath, "dot_config")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "bat"), []byte("test content"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"init"},
		{"add", "."},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
	}

	repo, err := Open(t.Context(), repoPath)
	if err != nil {
		t.Fatalf("failed to open repository: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	return repo, ref
}

func TestTreePathForwardSlashes(t *testing.T) {
	repo, ref := setupTestRepo(t)

	// TreePath should clean the double slash and resolve the directory.
	tree, err := repo.TreePath(ref, "dot_config//")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}

	entries, err := tree.Entries()
	if err != nil {
		t.Fatalf("failed to get entries: %v", err)
	}

	for _, e := range entries {
		path := e.File().Path()
		if strings.Contains(path, `\`) {
			t.Errorf("entry path contains backslash: %q", path)
		}
	}
}

func TestTreeEntryPathForwardSlashes(t *testing.T) {
	repo, ref := setupTestRepo(t)

	tree, err := repo.TreePath(ref, "dot_config")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}

	entries, err := tree.Entries()
	if err != nil {
		t.Fatalf("failed to get entries: %v", err)
	}

	for _, e := range entries {
		path := e.File().Path()
		if strings.Contains(path, `\`) {
			t.Errorf("entry path contains backslash: %q", path)
		}
		if !strings.Contains(path, "/") {
			t.Errorf("expected forward slash in path: %q", path)
		}
	}
}

// TestOpenReportsGitReason checks that when git refuses a repository the
// error still matches ErrNotAGitRepository but also says why.
func TestOpenReportsGitReason(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo.git")
	cmd := exec.CommandContext(context.Background(), "git", "init", "--bare", repoPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	// Makes git treat the repository as owned by someone else, as it would
	// on a volume owned by another user. System and global config are not
	// read, so a machine-wide safe.directory cannot accept the repository.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	_, err := Open(t.Context(), repoPath)
	if !errors.Is(err, ErrNotAGitRepository) {
		t.Fatalf("Open error = %v, want ErrNotAGitRepository", err)
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("Open error = %q, want git's reason (dubious ownership)", err)
	}
}

// A repository without references has an empty list of them, not an error.
func TestReferencesEmpty(t *testing.T) {
	repo, err := Init(t.Context(), filepath.Join(t.TempDir(), "empty"), true)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := repo.References()
	if err != nil || len(refs) != 0 {
		t.Fatalf("References of an empty repository = %v, %v; want none", refs, err)
	}
}

// runGit runs git in dir with a fixed identity and no line ending conversion.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "core.autocrlf=false",
	}, args...)
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func evalPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenLocal checks that "soft browse" can open a repository from any
// directory inside it, from a linked worktree and from a bare repository,
// where Open only accepts the repository root.
func TestOpenLocal(t *testing.T) {
	repo, _ := setupTestRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo.Path, "worktree", "add", "-q", wt)
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, t.TempDir(), "clone", "-q", "--bare", repo.Path, bare)

	for _, tc := range []struct {
		name, path, root string
		bare             bool
	}{
		{"root", repo.Path, repo.Path, false},
		{"subdirectory", filepath.Join(repo.Path, "dot_config"), repo.Path, false},
		{"linked worktree", wt, wt, false},
		{"linked worktree subdirectory", filepath.Join(wt, "dot_config"), wt, false},
		{"bare", bare, bare, true},
		{"bare subdirectory", filepath.Join(bare, "refs"), bare, true},
		{"git directory", filepath.Join(repo.Path, ".git"), filepath.Join(repo.Path, ".git"), true},
		{"git directory subdirectory", filepath.Join(repo.Path, ".git", "refs"), filepath.Join(repo.Path, ".git"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := OpenLocal(t.Context(), tc.path)
			if err != nil {
				t.Fatalf("OpenLocal(%q): %v", tc.path, err)
			}
			if got, want := evalPath(t, r.Path), evalPath(t, tc.root); got != want {
				t.Errorf("Path = %q, want %q", got, want)
			}
			if r.IsBare != tc.bare {
				t.Errorf("IsBare = %v, want %v", r.IsBare, tc.bare)
			}
			if _, err := r.HEAD(); err != nil {
				t.Errorf("HEAD: %v", err)
			}
		})
	}

	t.Run("not a repository", func(t *testing.T) {
		_, err := OpenLocal(t.Context(), t.TempDir())
		if !errors.Is(err, ErrNotAGitRepository) {
			t.Fatalf("OpenLocal error = %v, want ErrNotAGitRepository", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenLocal(t.Context(), f)
		if !errors.Is(err, ErrNotAGitRepository) || !strings.Contains(err.Error(), "is not a directory") {
			t.Fatalf("OpenLocal error = %v, want ErrNotAGitRepository: ... is not a directory", err)
		}
	})
}

// TestLsTreeRejectsBadRefs checks that a ref that is not a single revision,
// or that looks like a rev-parse option, is reported as a missing revision
// instead of panicking or reaching rev-parse as an option.
func TestLsTreeRejectsBadRefs(t *testing.T) {
	t.Parallel()
	repo, ref := setupTestRepo(t)

	for _, bad := range []string{
		"^HEAD",
		"^" + ref.ID(),
		"HEAD^@",
		"HEAD^!",
		"--git-dir",
		"--",
		"-- --git-dir",
		"HEAD..HEAD",
		"nope",
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			if _, err := repo.LsTree(bad); !errors.Is(err, ErrRevisionNotExist) {
				t.Fatalf("LsTree(%q) error = %v, want ErrRevisionNotExist", bad, err)
			}
		})
	}

	for _, good := range []string{"HEAD", ref.ID(), ref.ID()[:7], ref.Name().Short() + "~0", "HEAD^{tree}"} {
		tree, err := repo.LsTree(good)
		if err != nil {
			t.Fatalf("LsTree(%q): %v", good, err)
		}
		if _, err := tree.TreeEntry("dot_config/bat"); err != nil {
			t.Fatalf("LsTree(%q).TreeEntry: %v", good, err)
		}
	}
}

// TestTreeIDMatchesExitCode checks that only git's exit status 128 means a
// missing revision. A git that fails otherwise is reported as it failed,
// with its stderr, even when that stderr happens to read like the text of
// an exit status 128.
func TestTreeIDMatchesExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as git")
	}
	repo, _ := setupTestRepo(t)

	bin := t.TempDir()
	script := "#!/bin/sh\necho 'fatal: exit status 128' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := repo.LsTree("HEAD")
	if err == nil {
		t.Fatal("LsTree succeeded with a git that fails")
	}
	if errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("exit status 1 reported as a missing revision: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("error = %v, want one that wraps git's *exec.ExitError with code 1", err)
	}
	if !strings.Contains(err.Error(), "fatal: exit status 128") {
		t.Errorf("error = %v, want git's stderr in it", err)
	}
}

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

// TestLsTreeCorruptRefsIsNotMissing checks that git failing to read a
// repository, here one with corrupt packed-refs, is not taken for a
// missing revision: both exit with 128, and only a revision git itself
// reports missing is an ErrRevisionNotExist.
func TestLsTreeCorruptRefsIsNotMissing(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	root := gittest.Commit(t, bare, gittest.Valid, gittest.Tree(t, bare, "f", "root\n"), "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")

	repo, err := Open(t.Context(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LsTree("nope"); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(nope) error = %v, want ErrRevisionNotExist", err)
	}

	gittest.Git(t, bare, nil, "pack-refs", "--all")
	if err := os.WriteFile(filepath.Join(bare, "packed-refs"), []byte("not a packed-refs file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", "nope"} {
		_, err := repo.LsTree(ref)
		if err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q) with corrupt refs: err = %v, want git's failure", ref, err)
		}
	}
}

// TestLsTreeMissingTreeIsNotMissingRevision checks that a commit whose tree
// object is gone from the repository is git failing to read it, not a
// revision the caller got wrong, while a revision that names no tree-ish,
// such as a blob, still is an ErrRevisionNotExist.
func TestLsTreeMissingTreeIsNotMissingRevision(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	tree := gittest.Tree(t, bare, "f", "root\n")
	root := gittest.Commit(t, bare, gittest.Valid, tree, "root")
	blob := gittest.Git(t, bare, nil, "rev-parse", tree+":f")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)

	repo, err := Open(t.Context(), bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LsTree(blob); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(blob) error = %v, want ErrRevisionNotExist", err)
	}

	if err := os.Remove(filepath.Join(bare, "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", root} {
		_, err := repo.LsTree(ref)
		if err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q) without its tree object: err = %v, want git's failure", ref, err)
		}
	}
	if _, err := repo.LsTree(blob); !errors.Is(err, ErrRevisionNotExist) {
		t.Fatalf("LsTree(blob) error = %v, want ErrRevisionNotExist", err)
	}
}

// TestMissingCommitObjectOfRef checks that a ref whose commit object is
// gone (a branch, an annotated tag, a detached HEAD) is the repository
// failing, for both the tree and the commit lookup, while a raw object ID
// that names nothing, an unknown name and a tag on a blob, lightweight or
// annotated, still are missing revisions.
func TestMissingCommitObjectOfRef(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "r.git")
	gittest.Git(t, filepath.Dir(bare), nil, "init", "-q", "--bare", bare)
	tree := gittest.Tree(t, bare, "f", "root\n")
	blob := gittest.Git(t, bare, nil, "rev-parse", tree+":f")
	root := gittest.Commit(t, bare, gittest.Valid, tree, "root")
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/main", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/tags/blobtag", blob)
	gittest.Git(t, bare, nil, "tag", "-a", "-m", "on a blob", "annblob", blob)
	gittest.Git(t, bare, nil, "tag", "-a", "-m", "on main", "anngone", root)
	// Names that start with a dash, which git would read as an option
	// without --end-of-options in namesRef and refUnreadable.
	gittest.Git(t, bare, nil, "update-ref", "refs/heads/-dash", root)
	gittest.Git(t, bare, nil, "update-ref", "refs/tags/-dashtag", blob)
	gittest.Git(t, bare, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	if err := os.Remove(filepath.Join(bare, "objects", root[:2], root[2:])); err != nil {
		t.Fatal(err)
	}

	detached := filepath.Join(t.TempDir(), "d.git")
	gittest.Git(t, filepath.Dir(detached), nil, "init", "-q", "--bare", detached)
	dcommit := gittest.Commit(t, detached, gittest.Valid, gittest.Tree(t, detached, "f", "root\n"), "root")
	gittest.Git(t, detached, nil, "update-ref", "--no-deref", "HEAD", dcommit)
	if err := os.Remove(filepath.Join(detached, "objects", dcommit[:2], dcommit[2:])); err != nil {
		t.Fatal(err)
	}
	drepo, err := Open(t.Context(), detached)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drepo.LsTree("HEAD"); err == nil || errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("LsTree(HEAD) of a detached HEAD without its commit: err = %v, want git's failure", err)
	}
	if _, err := drepo.CommitByRevision("HEAD"); err == nil || errors.Is(err, ErrRevisionNotExist) {
		t.Errorf("CommitByRevision(HEAD) of a detached HEAD without its commit: err = %v, want git's failure", err)
	}

	repo, err := Open(t.Context(), bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"main", "refs/heads/main", "HEAD", "anngone", "-dash"} {
		if _, err := repo.LsTree(ref); err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q): err = %v, want git's failure", ref, err)
		}
		if _, err := repo.CommitByRevision(ref); err == nil || errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q): err = %v, want git's failure", ref, err)
		}
	}
	for _, rev := range []string{root, "nope", "main~1", "blobtag", "annblob", "-dashtag"} {
		if _, err := repo.LsTree(rev); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("LsTree(%q): err = %v, want ErrRevisionNotExist", rev, err)
		}
		if _, err := repo.CommitByRevision(rev); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("CommitByRevision(%q): err = %v, want ErrRevisionNotExist", rev, err)
		}
	}
}
