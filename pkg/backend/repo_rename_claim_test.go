package backend

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// renameHookStore runs onRename when a rename is recorded, in the middle of
// RenameRepository, so a test can act as a concurrent request there.
type renameHookStore struct {
	store.Store
	onRename func()
}

func (s renameHookStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	s.onRename()
	return s.Store.SetRepoNameByName(ctx, h, name, newName)
}

// A create or import that claims the new name while a rename is under way
// fails, instead of having its empty directory replaced by the renamed
// repository and its cleanup then deleting that repository.
func TestRenameRepositoryClaimsNewPath(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var claimErr error
	be.store = renameHookStore{Store: be.store, onRename: func() {
		claimErr = claimRepoPath(be.repoPath("c"))
	}}
	if err := be.RenameRepository(ctx, "a", "c"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimErr, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(claimErr, proto.ErrRepoExist)")
	} // the concurrent claim failed

	r, err := be.Repository(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// A rename that fails after claiming the new path gives the claim up and
// leaves the repository where it was.
func TestRenameRepositoryFailureReleasesClaim(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	failed := errors.New("store failure")
	be.store = failingRenameStore{Store: be.store, err: failed}
	if !errors.Is(be.RenameRepository(ctx, "a", "c"), proto.ErrServerFault) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"c\"), proto.ErrServerFault)")
	} // the cause is logged, not returned

	_, err = os.Lstat(be.repoPath("c"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, os.ErrNotExist)")
	} // the claim was given up
	r, err := be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

type failingRenameStore struct {
	store.Store
	err error
}

func (s failingRenameStore) SetRepoNameByName(context.Context, db.Handler, string, string) error {
	return s.err
}
