package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	repo, err := Open(repoPath)
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

	_, err := Open(repoPath)
	if !errors.Is(err, ErrNotAGitRepository) {
		t.Fatalf("Open error = %v, want ErrNotAGitRepository", err)
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("Open error = %q, want git's reason (dubious ownership)", err)
	}
}

// A repository without references has an empty list of them, not an error.
func TestReferencesEmpty(t *testing.T) {
	repo, err := Init(filepath.Join(t.TempDir(), "empty"), true)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := repo.References()
	if err != nil || len(refs) != 0 {
		t.Fatalf("References of an empty repository = %v, %v; want none", refs, err)
	}
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
			r, err := OpenLocal(tc.path)
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
		_, err := OpenLocal(t.TempDir())
		if !errors.Is(err, ErrNotAGitRepository) {
			t.Fatalf("OpenLocal error = %v, want ErrNotAGitRepository", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenLocal(f)
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
		"^" + ref.ID,
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

	for _, good := range []string{"HEAD", ref.ID, ref.ID[:7], ref.Name().Short() + "~0", "HEAD^{tree}"} {
		tree, err := repo.LsTree(good)
		if err != nil {
			t.Fatalf("LsTree(%q): %v", good, err)
		}
		if _, err := tree.TreeEntry("dot_config/bat"); err != nil {
			t.Fatalf("LsTree(%q).TreeEntry: %v", good, err)
		}
	}
}
