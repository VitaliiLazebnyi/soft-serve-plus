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

// failingLookupStore counts its GetRepoByName lookups and fails only the
// failAt-th one (counting from 1; 0 fails none).
type failingLookupStore struct {
	store.Store
	failAt int32
	calls  atomic.Int32
}

var errLookup = errors.New("lookup failed")

func (s *failingLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.calls.Add(1) == s.failAt {
		return models.Repo{}, errLookup
	}
	return s.Store.GetRepoByName(ctx, h, name)
}

// GetRepoByNameForUpdate counts and fails as GetRepoByName does.
func (s *failingLookupStore) GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	if s.calls.Add(1) == s.failAt {
		return models.Repo{}, errLookup
	}
	return s.Store.GetRepoByNameForUpdate(ctx, h, name)
}

// A lookup that fails for any reason other than a missing row says nothing
// about whether the repository exists, so its directory must stay, not be
// removed as an orphan with the row kept. Only the delete's own
// lookup fails: a failure that is gone by the next lookup must still be
// returned, not taken for an orphan and reported as not found.
func TestDeleteRepositoryLookupErrorKeepsDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))

	// Repository, which builds the webhook event, looks the row up first;
	// the second lookup is the delete's own.
	st := &failingLookupStore{Store: be.store, failAt: 2}
	be.store = st

	if err := be.DeleteRepository(ctx, "delrepo"); !errors.Is(err, errLookup) {
		t.Fatalf("DeleteRepository => %v, want the lookup error", err)
	}
	if n := st.calls.Load(); n != 2 {
		t.Fatalf("DeleteRepository made %d lookups, want 2", n)
	}

	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory removed after a failed lookup: %v", err)
	}
	if _, err := be.Repository(ctx, "delrepo"); err != nil {
		t.Errorf("repository gone after a failed lookup: %v", err)
	}
}

// Deleting a case variant of a repository's name must not remove that
// repository's directory, which a case-insensitive filesystem resolves the
// variant to.
func TestDeleteRepositoryCaseVariantKeepsDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := asAdmin(t, be, backendTestContext(be, be.cfg))

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

// A nested name with a directory and no row is not found either, whether
// spelled as on disk or as a case variant, and its directory stays.
func TestDeleteRepositoryLeavesNestedOrphan(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	rp := be.repoPath("org/orphan")
	if err := os.MkdirAll(rp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Org/orphan", "org/orphan"} {
		if err := be.DeleteRepository(ctx, name); !errors.Is(err, proto.ErrRepoNotFound) {
			t.Errorf("DeleteRepository(%s) => %v, want ErrRepoNotFound", name, err)
		}
		if _, err := os.Stat(rp); err != nil {
			t.Fatalf("orphan removed through %s: %v", name, err)
		}
	}
}
