package database

import (
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/dbtest"
)

// The webhook cap holds when the database defaults to REPEATABLE READ. A
// create that waited for LockAndCountWebhooks' row lock must count the
// webhook the holder inserted; under REPEATABLE READ its snapshot predates
// the wait, so it counted one fewer and a second create passed the cap.
func TestLockAndCountWebhooksSeesHolderUnderRepeatableReadPostgres(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	ctx, dbx := newWebhookTestDB(t)
	dbtest.SetDefaultIsolation(ctx, t, dbx, "repeatable read")
	s := New(nil)

	type result struct {
		n   int
		err error
	}
	waiter := make(chan result, 1)
	err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(ctx, &holder, `SELECT pg_backend_pid()`); err != nil {
			return err
		}
		if n, err := s.LockAndCountWebhooks(ctx, tx, 7); err != nil || n != 0 {
			t.Errorf("LockAndCountWebhooks() = %d, %v; want 0", n, err)
		}

		go func() {
			var r result
			r.err = dbx.TransactionContext(ctx, func(tx *db.Tx) error {
				var err error
				r.n, err = s.LockAndCountWebhooks(ctx, tx, 7)
				return err
			})
			waiter <- r
		}()

		// Insert only once the other transaction waits for the lock.
		deadline := time.Now().Add(time.Minute)
		for {
			var blocked bool
			if err := dbx.GetContext(ctx, &blocked,
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, holder); err != nil {
				return err
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Error("the second LockAndCountWebhooks did not wait for the lock")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, err := s.CreateWebhook(ctx, tx, 7, "https://93.184.216.34/a", "", 0, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	r := <-waiter
	if r.err != nil || r.n != 1 {
		t.Errorf("LockAndCountWebhooks() after waiting = %d, %v; want 1", r.n, r.err)
	}
}
