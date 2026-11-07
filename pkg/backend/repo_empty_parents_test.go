package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// nestedReposFixture returns a backend, a context whose user is an admin, and
// the repos directory.
func nestedReposFixture(t *testing.T) (*Backend, func(string), string) {
	t.Helper()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) {
		t.Helper()
		if _, err := be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	return be, create, filepath.Join(cfg.DataPath, "repos")
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still exists (err %v)", p, err)
	}
}

func assertDir(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() {
		t.Errorf("%s is not a directory: %v", p, err)
	}
}

// Deleting the last repository of a group removes the group directories it
// leaves empty, but not the repos directory or a group that still has
// repositories.
func TestDeleteRepositoryRemovesEmptyParents(t *testing.T) {
	be, create, root := nestedReposFixture(t)
	ctx := backendTestContext(be, be.cfg)
	create("a/b/c")
	create("a/d")

	if err := be.DeleteRepository(ctx, "a/b/c"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a", "b"))
	assertDir(t, filepath.Join(root, "a"))

	if err := be.DeleteRepository(ctx, "a/d"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a"))
	assertDir(t, root)

	// A top-level repository has no group to remove.
	create("top")
	if err := be.DeleteRepository(ctx, "top"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, root)
}

// A group directory holding something other than repositories is kept.
func TestDeleteRepositoryKeepsNonEmptyParent(t *testing.T) {
	be, create, root := nestedReposFixture(t)
	ctx := backendTestContext(be, be.cfg)
	create("a/b")
	note := filepath.Join(root, "a", "README")
	if err := os.WriteFile(note, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("file in group directory removed: %v", err)
	}
}

// A group directory that is a symlink is left alone, even when what it
// points to is empty.
func TestDeleteRepositoryKeepsSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	be, create, root := nestedReposFixture(t)
	ctx := backendTestContext(be, be.cfg)
	target := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	create("linked/b")

	if err := be.DeleteRepository(ctx, "linked/b"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "linked")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("symlinked group directory removed or replaced: %v", err)
	}
	assertDir(t, target)
}

// Removing an orphan directory (one with no row) also removes the group
// directories it leaves empty.
func TestDeleteOrphanRepositoryRemovesEmptyParents(t *testing.T) {
	be, _, root := nestedReposFixture(t)
	ctx := backendTestContext(be, be.cfg)
	if err := os.MkdirAll(be.repoPath("x/y/orphan"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "x/y/orphan"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "x"))
}

// Renaming a repository out of a group removes the group directories the old
// name leaves empty, and renaming within a group keeps the group.
func TestRenameRepositoryRemovesEmptyParents(t *testing.T) {
	be, create, root := nestedReposFixture(t)
	ctx := backendTestContext(be, be.cfg)
	create("a/b/c")

	if err := be.RenameRepository(ctx, "a/b/c", "a/b/d"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, filepath.Join(root, "a", "b"))

	if err := be.RenameRepository(ctx, "a/b/d", "e/f"); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Join(root, "a"))
	if _, err := os.Stat(filepath.Join(be.repoPath("e/f"), "HEAD")); err != nil {
		t.Errorf("renamed repository missing: %v", err)
	}

	// Renaming into a subgroup of the old group: the old group is not
	// empty afterwards, so it stays.
	if err := be.RenameRepository(ctx, "e/f", "e/g/h"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, filepath.Join(root, "e", "g"))
}

// A create that fails after claiming its directory removes the group
// directories it made.
func TestFailedCreateRemovesEmptyParents(t *testing.T) {
	be, create, root := nestedReposFixture(t)
	create("g/h/r")
	// Leave the row but take the directory away, so a second create
	// claims the path and then fails on the duplicate row.
	if err := os.RemoveAll(filepath.Join(root, "g")); err != nil {
		t.Fatal(err)
	}
	ctx := backendTestContext(be, be.cfg)
	if _, err := be.CreateRepository(ctx, "g/h/r", nil, proto.RepositoryOptions{}); !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("create error = %v, want ErrRepoExist", err)
	}
	assertGone(t, filepath.Join(root, "g"))
}

// withRepoParent recreates a parent removed between its creation and the
// placement, as a concurrent delete of the group's last repository does.
func TestWithRepoParentRecreatesRemovedParent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a", "b", "r.git")
	removed := false
	err := withRepoParent(p, func() error {
		if !removed {
			removed = true
			if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
				t.Fatal(err)
			}
		}
		return os.Mkdir(p, 0o755)
	})
	if err != nil {
		t.Fatalf("withRepoParent: %v", err)
	}
	assertDir(t, p)
}
