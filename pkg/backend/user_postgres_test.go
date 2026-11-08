package backend

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
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
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	owner, err := e.be.CreateUser(e.ctx, "owner", proto.UserOptions{})
	is.NoErr(err)

	result := make(chan error, 1)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return err
		}
		if err := e.be.store.CreateRepo(e.ctx, tx, "owned", owner.ID(), "", "", false, false, false, false); err != nil {
			return err
		}
		go func() {
			result <- e.be.DeleteUser(e.ctx, "owner")
		}()
		// An error, rather than a failed test, rolls the create back and
		// lets DeleteUser finish before the database is dropped.
		return waitBlockedBy(e, holder, result, "DeleteUser")
	}))

	select {
	case err := <-result:
		is.True(errors.Is(err, proto.ErrUserOwnsRepos))
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteUser still waiting after the create committed")
	}
	_, err = e.be.User(e.ctx, "owner")
	is.NoErr(err)
	m, err := e.be.store.GetRepoByName(e.ctx, e.be.db, "owned")
	is.NoErr(err)
	is.Equal(m.UserID.Int64, owner.ID())
}
