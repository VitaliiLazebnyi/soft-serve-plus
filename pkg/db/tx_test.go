package db_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// TestTransactionContextCancelledBeforeCommit checks that a transaction whose
// context is cancelled after fn has written, so database/sql rolls it back
// before Commit, is reported as failed rather than committed.
func TestTransactionContextCancelledBeforeCommit(t *testing.T) {
	d, err := test.Open(context.Background(), t)
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
		// finds it already done.
		deadline := time.Now().Add(5 * time.Second)
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
