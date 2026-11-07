package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// failingLookupStore fails GetRepoByName once fail is set and the lookups
// before it have been let through.
type failingLookupStore struct {
	store.Store
	pass atomic.Int32
	fail atomic.Bool
}

var errLookup = errors.New("lookup failed")

func (s *failingLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.fail.Load() && s.pass.Add(-1) < 0 {
		return models.Repo{}, errLookup
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// A lookup that fails for any reason other than a missing row says nothing
// about whether the repository exists, so its directory must stay. It used
// to be removed as an orphan, with the row kept.
func TestDeleteRepositoryLookupErrorKeepsDirectory(t *testing.T) {
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, be.cfg)

	st := &failingLookupStore{Store: be.store}
	be.store = st
	// Repository, which builds the webhook event, looks the row up first.
	st.pass.Store(1)
	st.fail.Store(true)

	if err := be.DeleteRepository(ctx, "delrepo"); !errors.Is(err, errLookup) {
		t.Fatalf("DeleteRepository => %v, want the lookup error", err)
	}
	st.fail.Store(false)

	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory removed after a failed lookup: %v", err)
	}
	if _, err := be.Repository(ctx, "delrepo"); err != nil {
		t.Errorf("repository gone after a failed lookup: %v", err)
	}
}

// A delete that finds no row while another delete has removed the row but
// not yet the directory has not deleted anything: it reports the repository
// as not found and leaves the directory to the other delete.
func TestDeleteRepositoryConcurrentDeleteNotFound(t *testing.T) {
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, be.cfg)

	// Stand in for the first delete between its commit and the removal of
	// the directory.
	release := be.repoDirs.hold("delrepo")
	if err := be.store.DeleteRepoByName(ctx, be.db, "delrepo"); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "delrepo"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("second DeleteRepository => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("second delete removed the directory: %v", err)
	}

	// Without the other delete, the directory is an orphan and goes.
	release()
	if err := be.DeleteRepository(ctx, "delrepo"); err != nil {
		t.Fatalf("DeleteRepository of the orphan => %v", err)
	}
	if _, err := os.Stat(rp); !os.IsNotExist(err) {
		t.Errorf("orphan directory still there (stat err: %v)", err)
	}
}

// A create or import claims its directory before recording the row; a
// delete in that window must not remove the directory being filled.
func TestDeleteRepositoryKeepsDirectoryOfCreateInProgress(t *testing.T) {
	be, _ := newTestBackend(t)
	ctx := backendTestContext(be, be.cfg)

	release := be.repoDirs.hold("pending")
	defer release()
	rp := be.repoPath("pending")
	if err := claimRepoPath(rp); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "pending"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("DeleteRepository => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("claimed directory removed: %v", err)
	}
}

// Deleting a case variant of a repository's name must not remove that
// repository's directory, which a case-insensitive filesystem resolves the
// variant to.
func TestDeleteRepositoryCaseVariantKeepsDirectory(t *testing.T) {
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, be.cfg)

	if err := be.DeleteRepository(ctx, "DelRepo"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("DeleteRepository of a case variant => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory removed by a case variant delete: %v", err)
	}
	if _, err := be.Repository(ctx, "delrepo"); err != nil {
		t.Errorf("repository gone after a case variant delete: %v", err)
	}
}

// A nested repository's orphan directory is still removed when every path
// component matches.
func TestDeleteRepositoryRemovesNestedOrphan(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	rp := be.repoPath("org/orphan")
	if err := os.MkdirAll(rp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(ctx, "Org/orphan"); !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatalf("DeleteRepository of a case variant => %v, want ErrRepoNotFound", err)
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("orphan removed through a case variant: %v", err)
	}
	if err := be.DeleteRepository(ctx, "org/orphan"); err != nil {
		t.Fatalf("DeleteRepository => %v", err)
	}
	if _, err := os.Stat(rp); !os.IsNotExist(err) {
		t.Errorf("orphan directory still there (stat err: %v)", err)
	}
}
