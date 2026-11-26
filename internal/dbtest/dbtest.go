// Package dbtest opens disposable databases for tests and names the ones
// tests create on a Postgres server themselves.
package dbtest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/lib/pq"
)

// OpenSqlite opens a new temp SQLite database for testing. It is a bare
// path, so foreign keys and the busy timeout are what db.Open adds to any
// data source, as for the server's.
// It removes the database file when the test is done using tb.Cleanup.
func OpenSqlite(ctx context.Context, tb testing.TB) (*db.DB, error) {
	dbx, _, err := managed(tb)(openSqlite(ctx, tb.TempDir()))
	return dbx, err
}

// Open opens a new, empty test database. It uses Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", creating a disposable database on the
// server named by SOFT_SERVE_DB_DATA_SOURCE (a postgres:// URL or a key=value
// connection string), and SQLite otherwise. The
// database is removed when the test is done.
//
// Locally, point the data source at the server's unix socket, such as
// "host=/path/to/socketdir port=5432 user=postgres sslmode=disable" (task
// test:postgres does): a full run opens thousands of database connections,
// and over TCP loopback on macOS that churn makes the kernel stall connects
// machine-wide until they fail with "operation timed out" (KNOWN_ISSUES
// item 309), the database's and the servers' under test alike.
func Open(ctx context.Context, tb testing.TB) (*db.DB, error) {
	dbx, _, err := OpenSource(ctx, tb)
	return dbx, err
}

// OpenSource is Open, and also returns the data source that opens the
// database with the driver dbx.DriverName(), for a config that must name
// the database it runs on.
func OpenSource(ctx context.Context, tb testing.TB) (*db.DB, string, error) {
	return managed(tb)(OpenUnmanaged(ctx, tb.TempDir()))
}

// OpenUnmanaged is OpenSource for a caller with no testing.TB, such as a
// TestMain: a SQLite database is a file in dir. The caller calls remove
// when it is done with the database, which closes it and drops it.
func OpenUnmanaged(ctx context.Context, dir string) (dbx *db.DB, dataSource string, remove func() error, err error) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		return openSqlite(ctx, dir)
	}
	return openPostgres(ctx, os.Getenv("SOFT_SERVE_DB_DATA_SOURCE"))
}

// managed returns a function that hands on what an opener returns and makes
// tb run its remove when the test is done.
func managed(tb testing.TB) func(*db.DB, string, func() error, error) (*db.DB, string, error) {
	return func(dbx *db.DB, dataSource string, remove func() error, err error) (*db.DB, string, error) {
		if err != nil {
			return nil, "", err
		}
		tb.Cleanup(func() {
			if err := remove(); err != nil {
				tb.Error(err)
			}
		})
		return dbx, dataSource, nil
	}
}

func openSqlite(ctx context.Context, dir string) (*db.DB, string, func() error, error) {
	dbpath := filepath.Join(dir, "test.db")
	dbx, err := db.Open(ctx, "sqlite", dbpath, db.Options{})
	if err != nil {
		return nil, "", nil, err
	}
	return dbx, dbpath, dbx.Close, nil
}

func openPostgres(ctx context.Context, dsn string) (*db.DB, string, func() error, error) {
	testDSN, drop, err := CreateDatabase(ctx, dsn)
	if err != nil {
		return nil, "", nil, err
	}
	dbx, err := db.Open(ctx, "postgres", testDSN, db.Options{})
	if err != nil {
		return nil, "", nil, errors.Join(err, drop())
	}
	remove := func() error {
		return errors.Join(dbx.Close(), drop())
	}
	return dbx, testDSN, remove, nil
}

// CreateDatabase creates a disposable database on the Postgres server that
// dsn, or the default local server when dsn is empty, names. It returns the
// data source for the new database, which is dsn with its database changed,
// and drop, which drops it. drop ends the connections left on the database
// first, such as those of a server that has not quite exited. No connection
// to the server stays open between the two.
func CreateDatabase(ctx context.Context, dsn string) (testDSN string, drop func() error, err error) {
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	admin, err := db.Open(ctx, "postgres", dsn, db.Options{})
	if err != nil {
		return "", nil, err
	}
	name, err := CreateDB(ctx, admin, "softserve_test")
	if err = errors.Join(err, admin.Close()); err != nil {
		return "", nil, err
	}
	drop = func() error {
		admin, err := db.Open(context.Background(), "postgres", dsn, db.Options{})
		if err != nil {
			return err
		}
		_, err = admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		return errors.Join(err, admin.Close())
	}
	if testDSN, err = withDatabase(dsn, name); err != nil {
		return "", nil, errors.Join(err, drop())
	}
	return testDSN, drop, nil
}

// SetDefaultIsolation makes level, such as "repeatable read", the default
// transaction isolation of the Postgres test database dbx, as a server,
// database or role setting can, for the connections dbx opens from now on.
// It closes dbx's idle connections, so that the next statements run on new
// ones. The test is skipped when the data source sets the default itself,
// which takes precedence.
func SetDefaultIsolation(ctx context.Context, tb testing.TB, dbx *db.DB, level string) {
	tb.Helper()
	var name string
	if err := dbx.GetContext(ctx, &name, `SELECT current_database()`); err != nil {
		tb.Fatal(err)
	}
	if _, err := dbx.ExecContext(ctx, "ALTER DATABASE "+pq.QuoteIdentifier(name)+
		" SET default_transaction_isolation = "+pq.QuoteLiteral(level)); err != nil {
		tb.Fatal(err)
	}
	// The setting applies to new sessions only: drop the pooled ones.
	dbx.SetMaxIdleConns(0)
	dbx.SetMaxIdleConns(2)
	// Checked outside a transaction: transactions run in READ COMMITTED
	// whatever the default.
	var got string
	if err := dbx.GetContext(ctx, &got, `SHOW default_transaction_isolation`); err != nil {
		tb.Fatal(err)
	}
	if got != level {
		tb.Skipf("the data source overrides the database default isolation level: got %q, want %q", got, level)
	}
}

// withDatabase returns dsn with its database set to name. dsn is either a
// URL (postgres://... or postgresql://..., in any letter case) or a key=value
// connection string such as "host=localhost user=postgres sslmode=disable".
// In a URL, a dbname query parameter would override the path, so it is
// removed. In the key=value form a later dbname overrides an earlier one, so
// it is appended.
func withDatabase(dsn, name string) (string, error) {
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(dsn) // also lowercases the scheme, which lib/pq requires
		if err != nil {
			return "", err
		}
		u.Path = "/" + name
		u.RawPath = ""
		q := u.Query()
		q.Del("dbname")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return strings.TrimSpace(dsn) + " dbname=" + name, nil
}
