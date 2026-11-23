package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/jmoiron/sqlx"
)

func TestOpenUnknownDriver(t *testing.T) {
	t.Parallel()
	_, err := Open(context.TODO(), "invalid", "", Options{})
	if err == nil {
		t.Error("Open(invalid, Options{}) => nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("Open(invalid, Options{}) => %v, want error containing 'unknown driver'", err)
	}
}

// waitingForWriter reports whether a goroutine waits for another write
// transaction: for its turn in DB.transaction, or for SQLite's write lock in
// SQLite's busy handler.
func waitingForWriter() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[select") && strings.HasPrefix(frames, "github.com/charmbracelet/soft-serve/pkg/db.(*DB).transaction(") {
			return true
		}
		if strings.Contains(frames, "modernc.org/sqlite/lib._sqliteDefaultBusyCallback(") {
			return true
		}
	}
	return false
}

// TestSqliteReadThenWriteWaitsForWriter checks that a transaction that reads
// before it writes waits (busy_timeout) for another connection's write
// transaction to commit instead of failing at once with "database is locked".
func TestSqliteReadThenWriteWaitsForWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A busy timeout of a minute, so that a loaded machine does not make
	// the wait for the writer run out.
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(60000)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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

	// Release the writer once the second transaction waits for it.
	deadline := time.Now().Add(time.Minute)
	for !waitingForWriter() {
		select {
		case err := <-readerErr:
			t.Fatalf("read-then-write transaction ended without waiting for the writer: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the read-then-write transaction did not wait for the writer's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
		cfg, err := postgresConfig(dsn, defaultPostgresConnectTimeout)
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
	cfg, err := postgresConfig("postgres://user@localhost/soft_serve", defaultPostgresConnectTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 4*time.Second {
		t.Errorf("ConnectTimeout with PGCONNECT_TIMEOUT=4 = %v, want 4s", cfg.ConnectTimeout)
	}
}

// Open gives up on a Postgres server that accepts the connection but never
// answers, instead of hanging startup, when the data source sets no
// timeout: it applies defaultPostgresConnectTimeout. The test goes through
// Open itself, so it waits out the real default (10 s).
func TestOpenPostgresSilentServer(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	os.Unsetenv("PGCONNECT_TIMEOUT") //nolint:errcheck

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

	// Without a connect timeout Open never returns, so the test waits for
	// it on a condition, for up to a minute, without timing it: a loaded
	// machine can take seconds past the timeout. The timeout's value is
	// TestPostgresConfigConnectTimeout's check.
	done := make(chan error, 1)
	go func() {
		_, err := Open(context.Background(), "postgres", "postgres://user@"+ln.Addr().String()+"/soft_serve?sslmode=disable", Options{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open on a silent server => nil error, want an error")
		}
	case <-time.After(time.Minute):
		t.Fatal("Open on a silent server did not return")
	}
}

// A panic in the transaction function rolls the transaction back, so its
// connection and SQLite's write lock are released, and the panic goes on.
func TestTransactionPanicRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(100)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(100)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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
	t.Parallel()
	d := &DB{x: sqlx.NewDb(sql.OpenDB(failingRollbackConnector{}), "sqlite")}
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

// TestSqliteReadTransactionDoesNotWaitForWriter checks that a read-only
// transaction does not take the write lock: it reads while another
// connection holds a write transaction open.
func TestSqliteReadTransactionDoesNotWaitForWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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

	// The writer holds the write lock until the read is done, so a read
	// that waits for it fails: at the SQLite lock with "database is locked"
	// after busy_timeout, or at the write turn when rctx ends. The deadline
	// is far longer than the read takes on a slow machine; it only bounds
	// how long a read that waits holds the test up.
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
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
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(50)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	d, err := Open(ctx, "sqlite", dsn, Options{})
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

	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond) // meant to run out while the writer holds its turn
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

	// The writer still holds the write lock and its turn, so a read that
	// waits for either fails, after busy_timeout or when rctx ends. The
	// deadline only bounds how long that takes.
	rctx, rcancel := context.WithTimeout(ctx, time.Minute)
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

// TestOpenResolvedSQLiteDataSource opens the data sources Validate resolves
// and checks that each database ends up where it should: a relative path or
// a relative "file:" URI in the data directory, and an in-memory database
// nowhere on disk.
func TestOpenResolvedSQLiteDataSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		dsn  string
		file string // expected database file in the data directory, or ""
	}{
		{"plain.db?_pragma=foreign_keys(1)", "plain.db"},
		{"file:uri.db?_pragma=foreign_keys(1)", "uri.db"},
		{":memory:?_pragma=foreign_keys(1)", ""},
		{"file::memory:?_pragma=foreign_keys(1)", ""},
	} {
		t.Run(c.dsn, func(t *testing.T) {
			t.Parallel()
			// A space and a '#' in the data path must survive a URI.
			dp := filepath.Join(t.TempDir(), "data dir#1")
			if err := os.MkdirAll(dp, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.DataPath = dp
			cfg.DB.DataSource = c.dsn
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}

			d, err := Open(ctx, "sqlite", cfg.DB.DataSource, Options{})
			if err != nil {
				t.Fatalf("open %q: %v", cfg.DB.DataSource, err)
			}
			defer d.Close() //nolint:errcheck
			if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
				t.Fatal(err)
			}

			entries, err := os.ReadDir(dp)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if c.file == "" {
				if len(names) != 0 {
					t.Errorf("data directory holds %q, want nothing for an in-memory database", names)
				}
				return
			}
			if _, err := os.Stat(filepath.Join(dp, c.file)); err != nil {
				t.Errorf("database not at %s (data directory holds %q): %v", c.file, names, err)
			}
		})
	}
}

func openPoolTestDB(t *testing.T, opts Options) *DB {
	t.Helper()
	d, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// holdConns checks out n connections at once and then returns them all to
// the pool.
func holdConns(t *testing.T, d *DB, n int) {
	t.Helper()
	conns := make([]*sql.Conn, 0, n)
	for range n {
		c, err := d.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestPoolLimitZeroKeepsDefaults(t *testing.T) {
	t.Parallel()
	d := openPoolTestDB(t, Options{})
	if got := d.Stats().MaxOpenConnections; got != 0 {
		t.Errorf("MaxOpenConnections = %d, want 0 (no limit)", got)
	}
	// database/sql keeps two idle connections by default. Calling
	// SetMaxIdleConns(0) would keep none.
	holdConns(t, d, 4)
	if got := d.Stats().Idle; got != 2 {
		t.Errorf("Idle = %d, want the database/sql default of 2", got)
	}
}

func TestPoolLimit(t *testing.T) {
	t.Parallel()
	d := openPoolTestDB(t, Options{MaxOpenConns: 5})
	if got := d.Stats().MaxOpenConnections; got != 5 {
		t.Errorf("MaxOpenConnections = %d, want 5", got)
	}
	holdConns(t, d, 5)
	if got := d.Stats().Idle; got != 2 {
		t.Errorf("Idle = %d, want the database/sql default of 2", got)
	}
}

func TestPostgresUTCConfigOverridesTimeZone(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"postgres://u@localhost/db?sslmode=disable",
		"postgres://u@localhost/db?sslmode=disable&timezone=Asia/Tokyo",
		"postgres://u@localhost/db?sslmode=disable&TimeZone=Asia/Tokyo&application_name=x",
		"host=localhost user=u dbname=db TimeZone=Asia/Tokyo",
	} {
		cfg, err := postgresUTCConfig(dsn)
		if err != nil {
			t.Fatalf("postgresUTCConfig(%q): %v", dsn, err)
		}
		for k, v := range cfg.Runtime {
			if strings.EqualFold(k, "timezone") && (k != "timezone" || v != "UTC") {
				t.Errorf("postgresUTCConfig(%q): run-time parameter %s=%s, want only timezone=UTC", dsn, k, v)
			}
		}
		if got := cfg.Runtime["timezone"]; got != "UTC" {
			t.Errorf("postgresUTCConfig(%q): timezone = %q, want UTC", dsn, got)
		}
		if strings.Contains(dsn, "application_name") && cfg.ApplicationName != "x" {
			t.Errorf("postgresUTCConfig(%q): application_name = %q, want x", dsn, cfg.ApplicationName)
		}
	}
}

// TestPostgresTimestampsAreUTC checks that a CURRENT_TIMESTAMP default stored
// in a TIMESTAMP column reads back as the current UTC time, even when the DSN
// asks for a session time zone far from UTC. It needs a Postgres server and
// runs only when SOFT_SERVE_DB_DRIVER is "postgres".
func TestPostgresTimestampsAreUTC(t *testing.T) {
	t.Parallel()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("SOFT_SERVE_DB_DRIVER is not postgres")
	}
	dsn := os.Getenv("SOFT_SERVE_DB_DATA_SOURCE")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	// Ask for a zone well away from UTC (and from the test machine's zone)
	// both in the DSN and through the options run-time parameter.
	const zone = "Pacific/Kiritimati" // UTC+14
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("TimeZone", zone)
		q.Set("options", "-c timezone="+zone)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn = strings.TrimSpace(dsn) + " TimeZone=" + zone + " options='-c timezone=" + zone + "'"
	}

	ctx := context.Background()
	d, err := Open(ctx, "postgres", dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	conn, err := d.x.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var tz string
	if err := conn.GetContext(ctx, &tz, "SHOW TimeZone"); err != nil {
		t.Fatal(err)
	}
	if tz != "UTC" {
		t.Errorf("session TimeZone = %q, want UTC", tz)
	}

	if _, err := conn.ExecContext(ctx, `CREATE TEMPORARY TABLE ts_test (
		id INTEGER,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err := conn.ExecContext(ctx, "INSERT INTO ts_test (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	var created time.Time
	if err := conn.GetContext(ctx, &created, "SELECT created_at FROM ts_test WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	// Allow for clock skew between the test and a server on the same host
	// or a container; a wrong zone is off by hours.
	const slack = time.Minute
	if created.Before(before.Add(-slack)) || created.After(after.Add(slack)) {
		t.Errorf("created_at = %v, want about %v", created, before.UTC())
	}
}

func TestSqliteDSN(t *testing.T) {
	t.Parallel()
	const added = "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	for dsn, want := range map[string]string{
		"soft-serve.db":                  "soft-serve.db?" + added,
		"file:soft-serve.db?mode=rwc":    "file:soft-serve.db?mode=rwc&" + added,
		":memory:":                       ":memory:?" + added,
		`C:\data\soft serve\soft.db`:     `C:\data\soft serve\soft.db?` + added,
		"soft-serve.db?_txlock=deferred": "soft-serve.db?_txlock=deferred&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)",
		// The server's default data source keeps its pragmas.
		"soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)": "soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		// A busy timeout the data source sets is kept, in any of its forms.
		"soft-serve.db?_pragma=busy_timeout(100)":     "soft-serve.db?_pragma=busy_timeout(100)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_pragma=BUSY_TIMEOUT = 0":      "soft-serve.db?_pragma=BUSY_TIMEOUT = 0&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_busy_timeout=100":             "soft-serve.db?_busy_timeout=100&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_timeout=100":                  "soft-serve.db?_timeout=100&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_fk=1&_pragma=busy_timeout(1)": "soft-serve.db?_fk=1&_pragma=busy_timeout(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_pragma=foreign_keys(on)":      "soft-serve.db?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate",
		// A journal mode the data source sets is kept.
		"soft-serve.db?_pragma=journal_mode(DELETE)": "soft-serve.db?_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"soft-serve.db?_pragma=Journal_Mode = wal":   "soft-serve.db?_pragma=Journal_Mode = wal&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"soft-serve.db?_journal_mode=DELETE":         "soft-serve.db?_journal_mode=DELETE&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"soft-serve.db?_journal=TRUNCATE":            "soft-serve.db?_journal=TRUNCATE&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		// A pragma that names the schema counts as the plain one.
		"soft-serve.db?_pragma=main.journal_mode(DELETE)": "soft-serve.db?_pragma=main.journal_mode(DELETE)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"soft-serve.db?_pragma=main.busy_timeout(100)":    "soft-serve.db?_pragma=main.busy_timeout(100)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		"soft-serve.db?_pragma=main.foreign_keys(1)":      "soft-serve.db?_pragma=main.foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate",
		// The driver passes mode and immutable to SQLite only in a "file:"
		// URI; it drops them from a plain path, which opens read-write.
		"soft-serve.db?mode=ro":     "soft-serve.db?mode=ro&" + added,
		"soft-serve.db?immutable=1": "soft-serve.db?immutable=1&" + added,
		// A read-only database cannot switch its journal mode.
		"file:soft-serve.db?mode=ro":     "file:soft-serve.db?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"file:soft-serve.db?immutable=1": "file:soft-serve.db?immutable=1&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		"file:soft-serve.db?immutable=0": "file:soft-serve.db?immutable=0&" + added,
		"file:soft-serve.db?mode=rw":     "file:soft-serve.db?mode=rw&" + added,
		// Starting with "?", the whole string is a file name to the driver.
		"?odd": "?odd",
	} {
		got, err := sqliteDSN(dsn)
		if err != nil || got != want {
			t.Errorf("sqliteDSN(%q) = %q, %v; want %q", dsn, got, err, want)
		}
	}
}

func TestSqliteDSNRefusesForeignKeysOff(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{
		"soft-serve.db?_pragma=foreign_keys(0)",
		"soft-serve.db?_pragma=foreign_keys=OFF",
		"soft-serve.db?_pragma=foreign_keys(1)&_fk=0",
		"soft-serve.db?_foreign_keys=false",
		"soft-serve.db?_pragma=foreign_keys(1)&_pragma=foreign_keys(no)",
		// The driver sorts the _pragma list, so an added foreign_keys(1)
		// would run first and this one would win.
		"soft-serve.db?_pragma=main.foreign_keys(0)",
		"soft-serve.db?_pragma=MAIN.Foreign_Keys = off",
	} {
		if _, err := sqliteDSN(dsn); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("sqliteDSN(%q) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
		if _, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), dsn), Options{}); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("Open(%q, Options{}) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
	}
}

// A _pragma whose name is quoted, bracketed or carries a comment is refused:
// SQLite reads each as the pragma it names, so it would get past the
// foreign-keys refusal or keep the added WAL from seeing a journal mode.
func TestSqliteDSNRefusesUnreadablePragmaNames(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		`"journal_mode"(DELETE)`,
		`main."journal_mode"(DELETE)`,
		`journal_mode/**/(DELETE)`,
		`foreign_keys/**/(0)`,
		`main."foreign_keys"(0)`,
		`main.[foreign_keys]=0`,
		"`foreign_keys`=0",
	} {
		dsn := "soft-serve.db?_pragma=" + p
		if _, err := sqliteDSN(dsn); !errors.Is(err, errSqlitePragmaName) {
			t.Errorf("sqliteDSN(%q) error = %v, want %v", dsn, err, errSqlitePragmaName)
		}
		if _, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), dsn), Options{}); !errors.Is(err, errSqlitePragmaName) {
			t.Errorf("Open(%q, Options{}) error = %v, want %v", dsn, err, errSqlitePragmaName)
		}
	}
}

// A data source that is a bare path, as SOFT_SERVE_DB_DATA_SOURCE often is,
// still gets foreign keys and a busy timeout on every connection. Without
// them, deleting a user would leave their keys behind, and any statement
// that met a git hook's write would fail at once with "database is locked".
func TestOpenSqliteBarePathEnforcesForeignKeysAndWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "soft.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck

	// Several connections at once, each set up by the driver.
	conns := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		c, err := d.x.Connx(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var fk, busy int
		if err := c.GetContext(ctx, &fk, "PRAGMA foreign_keys"); err != nil {
			t.Fatal(err)
		}
		if err := c.GetContext(ctx, &busy, "PRAGMA busy_timeout"); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy != defaultSqliteBusyTimeout {
			t.Errorf("foreign_keys = %d, busy_timeout = %d; want 1, %d", fk, busy, defaultSqliteBusyTimeout)
		}
	}
	for _, c := range conns {
		c.Close() //nolint:errcheck
	}

	// And deleting a row deletes the rows that reference it.
	for _, q := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY)",
		"CREATE TABLE public_keys (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE)",
		"INSERT INTO users (id) VALUES (1)",
		"INSERT INTO public_keys (id, user_id) VALUES (1, 1)",
		"DELETE FROM users WHERE id = 1",
	} {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM public_keys"); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("keys of a deleted user = %d, want 0", n)
	}
}

// Write-ahead logging is the default, as the README describes: a database
// made in rollback-journal mode switches to it on the next open and keeps
// its rows, foreign keys and busy timeout, and a data source that names
// another journal mode, as one on a network file system must, gets that mode
// instead.
func TestOpenSqliteWALDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "soft.db")
	open := func(dsn string) *DB {
		t.Helper()
		d, err := Open(ctx, "sqlite", dsn, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	mode := func(d *DB) string {
		t.Helper()
		var m string
		if err := d.GetContext(ctx, &m, "PRAGMA journal_mode"); err != nil {
			t.Fatal(err)
		}
		return m
	}

	d := open(path + "?_pragma=journal_mode(DELETE)")
	if m := mode(d); m != "delete" {
		t.Errorf("journal mode asked for DELETE = %q, want delete", m)
	}
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	d.Close() //nolint:errcheck

	d = open(path)
	var fk, busy, n int
	if err := d.GetContext(ctx, &fk, "PRAGMA foreign_keys"); err != nil {
		t.Fatal(err)
	}
	if err := d.GetContext(ctx, &busy, "PRAGMA busy_timeout"); err != nil {
		t.Fatal(err)
	}
	if err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil {
		t.Fatal(err)
	}
	if m := mode(d); m != "wal" || fk != 1 || busy != defaultSqliteBusyTimeout || n != 1 {
		t.Errorf("default: journal_mode %q, foreign_keys %d, busy_timeout %d, rows %d; want wal, 1, %d, 1",
			m, fk, busy, n, defaultSqliteBusyTimeout)
	}
	d.Close() //nolint:errcheck

	d = open(path + "?_pragma=journal_mode(DELETE)")
	defer d.Close() //nolint:errcheck
	if m := mode(d); m != "delete" {
		t.Errorf("journal mode asked for DELETE after WAL = %q, want delete", m)
	}
}

// TestSqliteReadDoesNotWaitForCommit checks that, with the server's default
// data source and with a bare path, a read goes on while another connection
// holds the lock a commit takes. In rollback-journal mode a commit locks the
// database file exclusively for all of its writes and syncs, and every read
// waits for it, for busy_timeout at most and then with "database is
// locked". On a slow disk a few commits in a row, such as a webhook
// delivery's record and prune, kept a push's repository lookup out for that
// long. BEGIN EXCLUSIVE takes the commit's lock at once and holds it until
// the test lets go; the read must not wait for that.
func TestSqliteReadDoesNotWaitForCommit(t *testing.T) {
	t.Parallel()
	for name, dsn := range map[string]string{
		"default": config.DefaultConfig().DB.DataSource,
		"bare":    "soft.db",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), dsn), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close() //nolint:errcheck
			for _, q := range []string{"CREATE TABLE t (n INTEGER)", "INSERT INTO t (n) VALUES (1)"} {
				if _, err := d.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}

			w, err := d.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close() //nolint:errcheck
			for _, q := range []string{"BEGIN EXCLUSIVE", "INSERT INTO t (n) VALUES (2)"} {
				if _, err := w.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}

			// The writer holds its lock until both reads are done.
			var n, m int
			if err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil {
				t.Errorf("read during a commit: %v", err)
			}
			if err := d.ReadTransactionContext(ctx, func(tx *Tx) error {
				return tx.GetContext(ctx, &m, "SELECT COUNT(*) FROM t")
			}); err != nil {
				t.Errorf("read transaction during a commit: %v", err)
			}
			if n != 1 || m != 1 {
				t.Errorf("reads during a commit saw %d and %d rows, want 1 (the committed ones)", n, m)
			}

			if _, err := w.ExecContext(ctx, "COMMIT"); err != nil {
				t.Fatal(err)
			}
			if err := d.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil || n != 2 {
				t.Errorf("read after the commit: %d rows, %v; want 2", n, err)
			}
		})
	}
}

// A journal mode set with the driver's own _journal_mode or _journal key, or
// with a _pragma that names the schema, is the data source's mode, as one
// set with a plain _pragma is. The driver applies
// those keys after the _pragma list, so a journal_mode(WAL) added next to
// them made every new connection switch the database to WAL and back. A
// switch needs the database to itself: a connection opened while another
// held a read transaction failed with "database is locked".
func TestOpenSqliteJournalModeKeys(t *testing.T) {
	t.Parallel()
	for name, query := range map[string]string{
		"_journal_mode":     "_journal_mode=DELETE",
		"_journal":          "_journal=DELETE",
		"main.journal_mode": "_pragma=main.journal_mode(DELETE)",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "soft.db")
			// A short busy timeout: the failure this checks for waits it out.
			d, err := Open(ctx, "sqlite", path+"?_pragma=busy_timeout(100)&"+query, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close() //nolint:errcheck
			if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
				t.Fatal(err)
			}

			// A read transaction holds its read lock on one connection
			// while the pool opens new ones.
			r, err := d.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			if _, err := r.ExecContext(ctx, "BEGIN"); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := r.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
				t.Fatal(err)
			}
			defer r.ExecContext(ctx, "ROLLBACK") //nolint:errcheck

			for range 2 {
				c, err := d.Conn(ctx)
				if err != nil {
					t.Fatalf("new connection during a read transaction: %v", err)
				}
				var m string
				err = c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&m)
				c.Close() //nolint:errcheck
				if err != nil || m != "delete" {
					t.Errorf("journal mode of a new connection = %q, %v; want delete", m, err)
				}
			}
		})
	}
}

// A read-only data source cannot switch the journal mode, so it gets no
// journal_mode(WAL) and opens a database in rollback-journal mode as it is.
func TestOpenSqliteReadOnlyKeepsJournalMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "soft.db")
	d, err := Open(ctx, "sqlite", path+"?_pragma=journal_mode(DELETE)", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE t (n INTEGER)", "INSERT INTO t (n) VALUES (1)"} {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close() //nolint:errcheck

	uri := "file:" + filepath.ToSlash(path)
	for _, q := range []string{"mode=ro", "immutable=1", "mode=ro&cache=private", "immutable=true"} {
		ro, err := Open(ctx, "sqlite", uri+"?"+q, Options{})
		if err != nil {
			t.Errorf("Open(%s): %v", q, err)
			continue
		}
		var n int
		var m string
		if err := ro.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil || n != 1 {
			t.Errorf("%s: read %d rows, %v; want 1", q, n, err)
		}
		if err := ro.GetContext(ctx, &m, "PRAGMA journal_mode"); err != nil || m != "delete" {
			t.Errorf("%s: journal mode %q, %v; want delete", q, m, err)
		}
		ro.Close() //nolint:errcheck
	}
}
