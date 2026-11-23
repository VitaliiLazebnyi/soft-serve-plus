package backend

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// Postgres runs transactions in READ COMMITTED, so DeleteUser looking for
// the user's repositories while a create has inserted one but not yet
// committed would miss it, and its DELETE, waiting for that insert's
// foreign key lock, would then remove the committed repository's row
// through ON DELETE CASCADE, leaving its directory behind. DeleteUser reads
// the user with FOR UPDATE, which waits for the insert to commit: here it
// must block on the creating transaction (checked with pg_blocking_pids)
// and then refuse with proto.ErrUserOwnsRepos.
func TestDeleteUserWaitsForRepoCreatePostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	owner, err := e.be.CreateUser(e.ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	if err := (e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return err
		}
		if _, err := e.be.store.CreateRepo(e.ctx, tx, models.Repo{
			Name:   "owned",
			UserID: owner.ID(),
		}); err != nil {
			return err
		}
		go func() {
			result <- e.be.DeleteUser(e.ctx, "owner")
		}()
		// An error, rather than a failed test, rolls the create back and
		// lets DeleteUser finish before the database is dropped.
		return waitBlockedBy(e, holder, result, "DeleteUser")
	})); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, proto.ErrUserOwnsRepos) {
			t.Fatal("not true: errors.Is(err, proto.ErrUserOwnsRepos)")
		}
	case <-time.After(time.Minute):
		t.Fatal("DeleteUser still waiting after the create committed")
	}
	_, err = e.be.User(e.ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.be.store.GetRepoByName(e.ctx, e.be.db, "owned")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.UserID, owner.ID(); got != want {
		t.Fatalf("m.UserID = %v, want %v", got, want)
	}
}
