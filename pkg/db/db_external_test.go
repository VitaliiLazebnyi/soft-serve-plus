package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// TestTransactionContextCancelledBeforeCommit checks that a transaction whose
// context is cancelled after fn has written, so database/sql rolls it back
// before Commit, is reported as failed rather than committed.
func TestTransactionContextCancelledBeforeCommit(t *testing.T) {
	t.Parallel()
	d, err := dbtest.Open(context.Background(), t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(), "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = d.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
			return err
		}
		cancel()
		// Wait for database/sql to roll the transaction back, so Commit
		// finds it already done. The bound only keeps a regression from
		// hanging the test; a slow machine can't reach it.
		deadline := time.Now().Add(time.Minute)
		for {
			_, err := tx.ExecContext(context.Background(), "SELECT 1")
			if errors.Is(err, sql.ErrTxDone) {
				return nil
			}
			if time.Now().After(deadline) {
				t.Error("transaction was not rolled back after its context was cancelled")
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err == nil {
		t.Fatal("TransactionContext = nil, want an error for a rolled-back transaction")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("TransactionContext = %v, want it to wrap context.Canceled", err)
	}

	var n int
	if err := d.GetContext(context.Background(), &n, "SELECT COUNT(*) FROM t"); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// Every transaction runs in READ COMMITTED on Postgres, whatever the
// server, database, role or data source make the default. The row locks the
// store takes (LockAndCountWebhooks, LockUserRepos, the ForUpdate reads) are
// followed by reads that must see what the previous holder of the lock
// committed, which only READ COMMITTED gives: a snapshot taken per
// statement.
func TestTransactionsAreReadCommittedPostgres(t *testing.T) {
	t.Parallel()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.SetDefaultIsolation(ctx, t, dbx, "repeatable read")

	for name, run := range map[string]func(context.Context, func(*db.Tx) error) error{
		"TransactionContext":     dbx.TransactionContext,
		"ReadTransactionContext": dbx.ReadTransactionContext,
	} {
		var got string
		if err := run(ctx, func(tx *db.Tx) error {
			return tx.GetContext(ctx, &got, `SHOW transaction_isolation`)
		}); err != nil {
			t.Fatal(err)
		}
		if got != "read committed" {
			t.Errorf("%s: isolation = %q, want read committed", name, got)
		}
	}
}
