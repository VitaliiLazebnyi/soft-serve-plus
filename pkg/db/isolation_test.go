package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Every transaction runs in READ COMMITTED on Postgres, whatever the
// server, database, role or data source make the default. The row locks the
// store takes (LockAndCountWebhooks, LockUserRepos, the ForUpdate reads) are
// followed by reads that must see what the previous holder of the lock
// committed, which only READ COMMITTED gives: a snapshot taken per
// statement.
func TestTransactionsAreReadCommittedPostgres(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	ctx := context.Background()
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	test.SetDefaultIsolation(ctx, t, dbx, "repeatable read")

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
