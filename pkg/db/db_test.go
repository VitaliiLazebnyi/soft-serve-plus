package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func TestOpenUnknownDriver(t *testing.T) {
	_, err := Open(context.TODO(), "invalid", "")
	if err == nil {
		t.Error("Open(invalid) => nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("Open(invalid) => %v, want error containing 'unknown driver'", err)
	}
}

func TestSqliteImmediateTx(t *testing.T) {
	for dsn, want := range map[string]string{
		"soft-serve.db": "soft-serve.db?_txlock=immediate",
		"soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)": "soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate",
		"file:soft-serve.db?mode=rwc":                                      "file:soft-serve.db?mode=rwc&_txlock=immediate",
		"soft-serve.db?_txlock=deferred":                                   "soft-serve.db?_txlock=deferred",
		"soft-serve.db?_pragma=foreign_keys(1)&_txlock=exclusive":          "soft-serve.db?_pragma=foreign_keys(1)&_txlock=exclusive",
	} {
		if got := sqliteImmediateTx(dsn); got != want {
			t.Errorf("sqliteImmediateTx(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// TestSqliteReadThenWriteWaitsForWriter checks that a transaction that reads
// before it writes waits (busy_timeout) for another connection's write
// transaction to commit instead of failing at once with "database is locked".
func TestSqliteReadThenWriteWaitsForWriter(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	d, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- d.TransactionContext(ctx, func(tx *Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
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

	readerErr := make(chan error, 1)
	go func() {
		readerErr <- d.TransactionContext(ctx, func(tx *Tx) error {
			var n int
			if err := tx.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (?)", n+1)
			return err
		})
	}()

	// Give the second transaction time to hit the lock before releasing.
	time.Sleep(200 * time.Millisecond)
	close(release)

	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if err := <-readerErr; err != nil {
		t.Fatalf("read-then-write transaction: %v", err)
	}

	var rows []int
	if err := d.SelectContext(ctx, &rows, "SELECT n FROM t ORDER BY n"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != 1 || rows[1] != 2 {
		t.Errorf("rows = %v, want [1 2]", rows)
	}
}

// A panic in the transaction function rolls the transaction back, so its
// connection and SQLite's write lock are released, and the panic goes on.
func TestTransactionPanicRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(100)"
	d, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Errorf("recovered %v, want boom", p)
			}
		}()
		_ = d.TransactionContext(ctx, func(tx *Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if n := d.Stats().InUse; n != 0 {
		t.Errorf("connections in use after panic = %d, want 0", n)
	}
	// Another write transaction can take the write lock.
	if err := d.TransactionContext(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (2)")
		return err
	}); err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	var rows []int
	if err := d.SelectContext(ctx, &rows, "SELECT n FROM t"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != 2 {
		t.Errorf("rows = %v, want [2]", rows)
	}
}

// A transaction whose fn calls runtime.Goexit, as t.FailNow does, is rolled
// back too: recover sees nothing in that case.
func TestTransactionGoexitRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(100)"
	d, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.TransactionContext(ctx, func(tx *Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
				return err
			}
			runtime.Goexit()
			return nil
		})
	}()
	<-done

	if n := d.Stats().InUse; n != 0 {
		t.Errorf("connections in use after Goexit = %d, want 0", n)
	}
	var rows []int
	if err := d.SelectContext(ctx, &rows, "SELECT n FROM t"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
}

// When the rollback fails too, both the error from fn and the rollback error
// stay reachable with errors.Is.
func TestRollbackErrorKeepsOriginal(t *testing.T) {
	d := &DB{DB: sqlx.NewDb(sql.OpenDB(failingRollbackConnector{}), "sqlite")}
	defer d.Close() //nolint:errcheck

	fnErr := errors.New("fn failed")
	err := d.TransactionContext(context.Background(), func(*Tx) error { return fnErr })
	if !errors.Is(err, fnErr) {
		t.Errorf("err = %v, want it to wrap %v", err, fnErr)
	}
	if !errors.Is(err, errRollback) {
		t.Errorf("err = %v, want it to wrap %v", err, errRollback)
	}
}

var errRollback = errors.New("rollback failed")

// failingRollbackConnector opens connections whose transactions fail to
// roll back.
type failingRollbackConnector struct{}

func (failingRollbackConnector) Connect(context.Context) (driver.Conn, error) {
	return failingRollbackConn{}, nil
}
func (failingRollbackConnector) Driver() driver.Driver { return nil }

type failingRollbackConn struct{}

func (failingRollbackConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (failingRollbackConn) Close() error              { return nil }
func (failingRollbackConn) Begin() (driver.Tx, error) { return failingRollbackTx{}, nil }

type failingRollbackTx struct{}

func (failingRollbackTx) Commit() error   { return nil }
func (failingRollbackTx) Rollback() error { return errRollback }
