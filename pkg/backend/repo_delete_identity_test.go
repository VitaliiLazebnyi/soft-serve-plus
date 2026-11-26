package backend

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// swapAfterLookupStore runs swap once, right after the first lookup of name
// by GetRepoByName, outside any transaction: what a rename and a create by
// another session in that moment would do.
type swapAfterLookupStore struct {
	store.Store
	name    string
	swapped atomic.Bool
	swap    func()
}

func (s *swapAfterLookupStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	m, err := s.Store.GetRepoByName(ctx, h, name)
	if name == s.name && s.swapped.CompareAndSwap(false, true) {
		s.swap()
	}
	return m, err
}

// swapFixture creates repository "victim", and returns a backend whose next
// lookup of it is followed by renaming it to "moved" and creating another
// "victim". It returns the ID of the original repository and a function
// that returns the ID of its replacement once the swap ran.
func swapFixture(t *testing.T) (be *Backend, ctx context.Context, orig proto.Repository, replacement func() int64) {
	t.Helper()
	be, cfg := newTestBackend(t)
	ctx = asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	orig, err := be.CreateRepository(ctx, "victim", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var replacementID int64
	st := &swapAfterLookupStore{Store: be.store, name: "victim"}
	st.swap = func() {
		if err := be.RenameRepository(ctx, "victim", "moved"); err != nil {
			t.Fatal(err)
		}
		r, err := be.CreateRepository(ctx, "victim", admin, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		replacementID = r.ID()
	}
	be.store = st
	return be, ctx, orig, func() int64 { return replacementID }
}

// A delete acts on the repository it looked up: if another repository took
// the name between that lookup and the delete, the delete reports the name
// as not found and leaves both alone.
func TestDeleteRepositoryActsOnTheRepositoryItLookedUp(t *testing.T) {
	t.Parallel()
	be, ctx, orig, replacement := swapFixture(t)

	err := be.DeleteRepository(ctx, "victim")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	r, err := be.Repository(ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.ID(), replacement(); got != want {
		t.Fatalf("r.ID() = %v, want %v", got, want)
	}
	moved, err := be.Repository(ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := moved.ID(), orig.ID(); got != want {
		t.Fatalf("moved.ID() = %v, want %v", got, want)
	}
}

// DeleteResolvedRepository deletes the repository a caller resolved, and
// checked access to, before; never another one that took its name since.
func TestDeleteResolvedRepository(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	admin := defaultAdmin(t, be)
	r, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.RenameRepository(ctx, "repo", "moved"); err != nil {
		t.Fatal(err)
	}
	other, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.DeleteResolvedRepository(ctx, r), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.DeleteResolvedRepository(ctx, r), proto.ErrRepoNotFound)")
	}
	cur, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cur.ID(), other.ID(); got != want {
		t.Fatalf("cur.ID() = %v, want %v", got, want)
	}

	if err := be.DeleteResolvedRepository(ctx, other); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "repo")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}
