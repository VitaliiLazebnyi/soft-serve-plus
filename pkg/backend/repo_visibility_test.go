package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestRepositoryReflectsDatabase verifies that access checks see a
// visibility change as soon as it is committed, whoever committed it
// (GHSA-f3fj-9625-rv3m).
func TestRepositoryReflectsDatabase(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := levelOf(t, be, ctx, "repo", nil); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, access.ReadOnlyAccess)
	}

	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.SetRepoIsPrivateByName(ctx, tx, "repo", true)
	}); err != nil {
		t.Fatal(err)
	}

	r, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got := levelOf(t, be, ctx, "repo", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, access.NoAccess)
	}
}
