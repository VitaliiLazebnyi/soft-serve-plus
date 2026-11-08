package backend

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// createCaseVariantFixture creates repository "secret" and reports whether
// the filesystem finds its directory under the case variant "SECRET" too.
func createCaseVariantFixture(t *testing.T, be *Backend) bool {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatal(err)
	}
	return be.repoPathExists("SECRET")
}

// assertSecretIntact checks that repository "secret" still has its row and
// its directory.
func assertSecretIntact(t *testing.T, be *Backend) {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	r, err := be.Repository(ctx, "secret")
	if err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if !r.IsPrivate() {
		t.Error("repository is no longer private")
	}
	if _, err := os.Stat(filepath.Join(be.repoPath("secret"), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}

// On a case-insensitive filesystem the path of "SECRET" is the directory of
// "secret". Deleting the case variant, which has no row, used to treat that
// directory as an orphan and remove it, leaving the real repository's row
// without a repository.
func TestDeleteRepositoryCaseVariantKeepsRepository(t *testing.T) {
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: SECRET is a different repository")
	}
	ctx := backendTestContext(be, cfg)

	if err := be.DeleteRepository(ctx, "SECRET"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(SECRET) => %v, want ErrRepoNotFound", err)
	}
	assertSecretIntact(t, be)
}

// On a case-insensitive filesystem renaming the case variant used to move the
// directory of "secret" while no row was renamed, stranding the repository.
func TestRenameRepositoryCaseVariantKeepsRepository(t *testing.T) {
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: SECRET is a different repository")
	}
	ctx := backendTestContext(be, cfg)

	if err := be.RenameRepository(ctx, "SECRET", "other"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("RenameRepository(SECRET) => %v, want ErrRepoNotFound", err)
	}
	assertSecretIntact(t, be)
	if be.repoPathExists("other") {
		t.Error("directory moved to the new name")
	}
	if _, err := be.Repository(ctx, "other"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("Repository(other) => %v, want ErrRepoNotFound", err)
	}
}

// Renaming a directory with no row used to move it without renaming any row,
// which reported success for a repository that does not exist. The rename now
// needs the row and leaves the directory where it is.
func TestRenameRepositoryWithoutRowLeavesDirectory(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	orphan := be.repoPath("orphan")
	is.NoErr(os.MkdirAll(orphan, 0o755))

	is.True(errors.Is(be.RenameRepository(ctx, "orphan", "other"), proto.ErrRepoNotFound))
	_, err := os.Stat(orphan)
	is.NoErr(err)
	is.True(!be.repoPathExists("other"))
}

// A directory with no row is still removed by a delete, so a leftover
// directory can be cleaned up.
func TestDeleteRepositoryRemovesOrphanDirectory(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	orphan := be.repoPath("orphan")
	is.NoErr(os.MkdirAll(orphan, 0o755))

	is.NoErr(be.DeleteRepository(ctx, "orphan"))
	_, err := os.Stat(orphan)
	is.True(os.IsNotExist(err))
	is.True(errors.Is(be.DeleteRepository(ctx, "orphan"), proto.ErrRepoNotFound))
}

// APFS ignores Unicode normalization as well as case, so the decomposed form
// of a repository's name leads to the directory of the precomposed one. Names
// like these are not case variants of each other, so the delete must compare
// the directory against every repository, not only names that fold alike.
func TestDeleteRepositoryNormalizationVariantKeepsRepository(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	const (
		precomposed = "\uD55C"             // 한
		decomposed  = "\u1112\u1161\u11AB" // 한 as jamo
	)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, precomposed, owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if !be.repoPathExists(decomposed) {
		t.Skip("filesystem keeps Unicode normalization forms apart")
	}

	if err := be.DeleteRepository(ctx, decomposed); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(decomposed) => %v, want ErrRepoNotFound", err)
	}
	if _, err := be.Repository(ctx, precomposed); err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(be.repoPath(precomposed), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}

// On a case-insensitive filesystem a repository can be named "Secret" while
// its directory is still spelled secret.git. Deleting "secret", which has no
// row and whose directory is spelled exactly so on disk, must not treat that
// directory as an orphan: it is the directory of "Secret".
func TestDeleteRepositoryExactSpellingOfAnotherRepositoryKeepsIt(t *testing.T) {
	be, cfg := newTestBackend(t)
	if !createCaseVariantFixture(t, be) {
		t.Skip("filesystem is case-sensitive: Secret has its own directory")
	}
	ctx := backendTestContext(be, cfg)

	if err := be.store.SetRepoNameByName(ctx, be.db, "secret", "Secret"); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "secret"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Errorf("DeleteRepository(secret) => %v, want ErrRepoNotFound", err)
	}
	if _, err := be.Repository(ctx, "Secret"); err != nil {
		t.Fatalf("repository row gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(be.repoPath("Secret"), "HEAD")); err != nil {
		t.Errorf("repository directory damaged: %v", err)
	}
}
