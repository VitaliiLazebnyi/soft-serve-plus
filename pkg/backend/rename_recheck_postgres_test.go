package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On Postgres the re-check after a failed rename waits for a transaction
// that holds the row's lock, as a COMMIT still in flight on the lost
// connection does, and sees the row as it leaves it. Here the rename's own
// transaction fails, and another transaction, standing in for the COMMIT
// in flight, renames the row to the new name and commits only once the
// re-check waits for it. A plain read saw the old name, moved the directory
// back, and left it away from its row.
func TestRenameRepositoryRecheckWaitsForRowLockPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	_, err := e.be.db.ExecContext(e.ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	e.be.store = unpinnableRenameStore{Store: e.be.store}

	committed := make(chan error, 1)
	e.be.renameTxDone = func(err error) error {
		// The rename's transaction has failed and rolled back. Hold the
		// row's lock with the rename that, from here, is in flight.
		tx, berr := beginRawTx(t, e.ctx, e.be.db)
		if berr != nil {
			committed <- berr
			return err
		}
		if _, xerr := tx.ExecContext(e.ctx, `UPDATE repos SET name = 'new' WHERE id = $1`, e.repo.ID()); xerr != nil {
			_ = tx.Rollback()
			committed <- xerr
			return err
		}
		go func() {
			// Commit once the re-check waits for the lock.
			deadline := time.Now().Add(time.Minute)
			for {
				var waiting bool
				if qerr := e.be.db.GetContext(context.Background(), &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock');`); qerr != nil {
					_ = tx.Rollback()
					committed <- qerr
					return
				}
				if waiting {
					committed <- tx.Commit()
					return
				}
				if time.Now().After(deadline) {
					_ = tx.Rollback()
					committed <- errors.New("the re-check did not wait for the row's lock")
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		return err
	}

	if err := e.be.RenameRepository(e.ctx, "repo", "new"); err != nil {
		t.Fatal(err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	r, err := e.be.Repository(e.ctx, "new")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the new name
	_, err = os.Stat(filepath.Join(e.be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// On Postgres too, when the outcome of a rename cannot be read either, the
// directory stays where it is rather than risk moving it away from its row.
// The re-check's locked lookup fails here; newTestBackend is SQLite only
// and never reaches it.
func TestRenameRepositoryCommitOutcomeUnknownLeavesDirectoryPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	e.be.store = failingByIDStore{Store: e.be.store}
	e.be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if !(e.be.RenameRepository(e.ctx, "repo", "new") != nil) {
		t.Fatal("not true: e.be.RenameRepository(e.ctx, \"repo\", \"new\") != nil")
	}
	_, err := os.Stat(filepath.Join(e.be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // where the committed row puts it
	_, err = os.Lstat(e.be.repoPath("repo"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}
