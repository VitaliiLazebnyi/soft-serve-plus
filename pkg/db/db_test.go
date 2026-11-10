package db

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestPostgresConfigConnectTimeout(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	os.Unsetenv("PGCONNECT_TIMEOUT") //nolint:errcheck
	for dsn, want := range map[string]time.Duration{
		"postgres://user@localhost:5432/soft_serve?sslmode=disable":                    defaultPostgresConnectTimeout,
		"postgres://user@localhost:5432/soft_serve?sslmode=disable&connect_timeout=3":  3 * time.Second,
		"postgres://user@localhost:5432/soft_serve?connect_timeout=0":                  0,
		"host=localhost port=5432 user=soft dbname=soft_serve sslmode=disable":         defaultPostgresConnectTimeout,
		"host=localhost user=soft dbname=soft_serve connect_timeout=7 sslmode=disable": 7 * time.Second,
	} {
		cfg, err := postgresConfig(dsn)
		if err != nil {
			t.Fatalf("postgresConfig(%q): %v", dsn, err)
		}
		if cfg.ConnectTimeout != want {
			t.Errorf("postgresConfig(%q).ConnectTimeout = %v, want %v", dsn, cfg.ConnectTimeout, want)
		}
		// The connect timeout must not drop the UTC session time zone.
		if got := cfg.Runtime["timezone"]; got != "UTC" {
			t.Errorf("postgresConfig(%q): timezone = %q, want UTC", dsn, got)
		}
	}

	// PGCONNECT_TIMEOUT is honoured as lib/pq honours it.
	t.Setenv("PGCONNECT_TIMEOUT", "4")
	cfg, err := postgresConfig("postgres://user@localhost/soft_serve")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 4*time.Second {
		t.Errorf("ConnectTimeout with PGCONNECT_TIMEOUT=4 = %v, want 4s", cfg.ConnectTimeout)
	}
}

// Open gives up on a Postgres server that accepts the connection but never
// answers, instead of hanging startup, when the data source sets no timeout.
func TestOpenPostgresSilentServer(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	os.Unsetenv("PGCONNECT_TIMEOUT") //nolint:errcheck
	old := defaultPostgresConnectTimeout
	defaultPostgresConnectTimeout = 300 * time.Millisecond
	t.Cleanup(func() { defaultPostgresConnectTimeout = old })

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		ln.Close() //nolint:errcheck
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close() //nolint:errcheck
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := Open(context.Background(), "postgres", "postgres://user@"+ln.Addr().String()+"/soft_serve?sslmode=disable")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open() on a silent server => nil error, want an error")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Open() took %v, want about the connect timeout", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Open() on a silent server did not return")
	}
}

// TestSqliteReadTransactionDoesNotWaitForWriter checks that a read-only
// transaction does not take the write lock: it reads while another
// connection holds a write transaction open.
func TestSqliteReadTransactionDoesNotWaitForWriter(t *testing.T) {
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
	if _, err := d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- d.TransactionContext(ctx, func(tx *Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (2)"); err != nil {
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

	// Well under busy_timeout, so a read that waits for the writer fails
	// instead of slowing the test down.
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n int
	readErr := d.ReadTransactionContext(rctx, func(tx *Tx) error {
		return tx.GetContext(rctx, &n, "SELECT COUNT(*) FROM t")
	})
	close(release)
	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if readErr != nil {
		t.Fatalf("read transaction: %v", readErr)
	}
	if n != 1 {
		t.Errorf("read transaction saw %d rows, want 1", n)
	}
}

// TestSqliteWritersTakeTurns checks that concurrent write transactions of
// one process all succeed when between them they hold the write lock far
// longer than busy_timeout, as a burst of writes does on a slow disk:
// they wait for their turn instead of failing at BEGIN with "database is
// locked".
func TestSqliteWritersTakeTurns(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(50)"
	d, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			errs <- d.TransactionContext(ctx, func(tx *Tx) error {
				if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (?)", i); err != nil {
					return err
				}
				// Hold the write lock as a slow commit would; all the
				// writers together hold it for several busy_timeouts.
				time.Sleep(20 * time.Millisecond)
				return nil
			})
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("writer: %v", err)
		}
	}

	var n int
	if err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil {
		t.Fatal(err)
	}
	if n != writers {
		t.Errorf("rows = %d, want %d", n, writers)
	}
}

// TestSqliteWriterWaitEndsWithContext checks that a write transaction
// waiting for its turn gives up when its context ends, and that reads do
// not wait for the turn at all.
func TestSqliteWriterWaitEndsWithContext(t *testing.T) {
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

	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	called := false
	err = d.TransactionContext(wctx, func(*Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting writer = %v, want context.DeadlineExceeded", err)
	}
	if called {
		t.Error("waiting writer ran fn without its turn")
	}

	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	var n int
	if err := d.ReadTransactionContext(rctx, func(tx *Tx) error {
		return tx.GetContext(rctx, &n, "SELECT COUNT(*) FROM t")
	}); err != nil {
		t.Errorf("read transaction: %v", err)
	}

	close(release)
	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
}
