package test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	sstest "github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/lib/pq"
)

// OpenSqlite opens a new temp SQLite database for testing, with the
// pragmas the server opens its database with: foreign keys enforced and a
// busy timeout.
// It removes the database file when the test is done using tb.Cleanup.
// If ctx is nil, context.TODO() is used.
func OpenSqlite(ctx context.Context, tb testing.TB) (*db.DB, error) {
	if ctx == nil {
		ctx = context.TODO()
	}
	dbpath := filepath.Join(tb.TempDir(), "test.db")
	dbx, err := db.Open(ctx, "sqlite", dbpath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	tb.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			tb.Error(err)
		}
	})
	return dbx, nil
}

// Open opens a new, empty test database. It uses Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", creating a disposable database on the
// server named by SOFT_SERVE_DB_DATA_SOURCE (a postgres:// URL or a key=value
// connection string), and SQLite otherwise. The
// database is removed when the test is done.
func Open(ctx context.Context, tb testing.TB) (*db.DB, error) {
	if ctx == nil {
		ctx = context.TODO()
	}
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		return OpenSqlite(ctx, tb)
	}
	return openPostgres(ctx, tb, os.Getenv("SOFT_SERVE_DB_DATA_SOURCE"))
}

func openPostgres(ctx context.Context, tb testing.TB, dsn string) (*db.DB, error) {
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	admin, err := db.Open(ctx, "postgres", dsn)
	if err != nil {
		return nil, err
	}
	name, err := sstest.CreateDB(ctx, admin, "softserve_test")
	if err != nil {
		admin.Close() //nolint:errcheck
		return nil, err
	}

	testDSN, err := withDatabase(dsn, name)
	if err != nil {
		admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
		admin.Close()                                                 //nolint:errcheck
		return nil, err
	}
	dbx, err := db.Open(ctx, "postgres", testDSN)
	if err != nil {
		admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)") //nolint:errcheck
		admin.Close()                                                 //nolint:errcheck
		return nil, err
	}
	tb.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			tb.Error(err)
		}
		if _, err := admin.ExecContext(context.TODO(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			tb.Error(err)
		}
		if err := admin.Close(); err != nil {
			tb.Error(err)
		}
	})
	return dbx, nil
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
