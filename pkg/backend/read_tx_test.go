package backend

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestLookupsDoNotWaitForWriter checks that the lookups behind every access
// check read without taking the SQLite write lock: they must succeed while
// another connection holds a write transaction open, instead of waiting for
// it (busy_timeout) and then failing with "database is locked".
func TestLookupsDoNotWaitForWriter(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	// Both differ from what the lookups fall back to on an error.
	is.NoErr(be.SetAnonAccess(ctx, access.ReadWriteAccess))
	is.NoErr(be.SetAllowKeyless(ctx, true))
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{Description: "desc"})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "collab", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadOnlyAccess)
	}))

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- be.db.TransactionContext(ctx, func(tx *db.Tx) error {
			if err := be.store.CreateUser(ctx, tx, "writer", false, nil); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	func() {
		defer close(release)
		// Well under busy_timeout, so a lookup that waits for the writer
		// fails instead of slowing the test down.
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		u, err := be.User(rctx, "admin")
		if err != nil {
			t.Errorf("User: %v", err)
		} else if _, err := be.UserByID(rctx, u.ID()); err != nil {
			t.Errorf("UserByID: %v", err)
		}
		if _, err := be.Users(rctx); err != nil {
			t.Errorf("Users: %v", err)
		}
		if _, err := be.ListPublicKeys(rctx, "admin"); err != nil {
			t.Errorf("ListPublicKeys: %v", err)
		}
		if got := be.AnonAccess(rctx); got != access.ReadWriteAccess {
			t.Errorf("AnonAccess = %v, want %v", got, access.ReadWriteAccess)
		}
		if !be.AllowKeyless(rctx) {
			t.Errorf("AllowKeyless = false, want true")
		}
		if level, ok, err := be.IsCollaborator(rctx, "repo", "collab"); err != nil || !ok || level != access.ReadOnlyAccess {
			t.Errorf("IsCollaborator = %v, %v, %v; want %v, true, nil", level, ok, err, access.ReadOnlyAccess)
		}
		if _, err := be.Collaborators(rctx, "repo"); err != nil {
			t.Errorf("Collaborators: %v", err)
		}
		if _, err := be.Repository(rctx, "repo"); err != nil {
			t.Errorf("Repository: %v", err)
		}
		if _, err := be.Repositories(rctx); err != nil {
			t.Errorf("Repositories: %v", err)
		}
		if _, err := be.Description(rctx, "repo"); err != nil {
			t.Errorf("Description: %v", err)
		}
	}()

	is.NoErr(<-writerErr)
}
