package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSqliteDSN(t *testing.T) {
	const added = "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	for dsn, want := range map[string]string{
		"soft-serve.db":                  "soft-serve.db?" + added,
		"file:soft-serve.db?mode=rwc":    "file:soft-serve.db?mode=rwc&" + added,
		":memory:":                       ":memory:?" + added,
		`C:\data\soft serve\soft.db`:     `C:\data\soft serve\soft.db?` + added,
		"soft-serve.db?_txlock=deferred": "soft-serve.db?_txlock=deferred&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		// The server's default data source keeps its pragmas.
		"soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)": "soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate",
		// A busy timeout the data source sets is kept, in any of its forms.
		"soft-serve.db?_pragma=busy_timeout(100)":     "soft-serve.db?_pragma=busy_timeout(100)&_pragma=foreign_keys(1)&_txlock=immediate",
		"soft-serve.db?_pragma=BUSY_TIMEOUT = 0":      "soft-serve.db?_pragma=BUSY_TIMEOUT = 0&_pragma=foreign_keys(1)&_txlock=immediate",
		"soft-serve.db?_busy_timeout=100":             "soft-serve.db?_busy_timeout=100&_pragma=foreign_keys(1)&_txlock=immediate",
		"soft-serve.db?_timeout=100":                  "soft-serve.db?_timeout=100&_pragma=foreign_keys(1)&_txlock=immediate",
		"soft-serve.db?_fk=1&_pragma=busy_timeout(1)": "soft-serve.db?_fk=1&_pragma=busy_timeout(1)&_txlock=immediate",
		"soft-serve.db?_pragma=foreign_keys(on)":      "soft-serve.db?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)&_txlock=immediate",
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
	for _, dsn := range []string{
		"soft-serve.db?_pragma=foreign_keys(0)",
		"soft-serve.db?_pragma=foreign_keys=OFF",
		"soft-serve.db?_pragma=foreign_keys(1)&_fk=0",
		"soft-serve.db?_foreign_keys=false",
		"soft-serve.db?_pragma=foreign_keys(1)&_pragma=foreign_keys(no)",
	} {
		if _, err := sqliteDSN(dsn); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("sqliteDSN(%q) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
		if _, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), dsn)); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("Open(%q) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
	}
}

// A data source that is a bare path, as SOFT_SERVE_DB_DATA_SOURCE often is,
// still gets foreign keys and a busy timeout on every connection. Both used
// to be off: deleting a user left their keys behind, and any statement that
// met a git hook's write failed at once with "database is locked".
func TestOpenSqliteBarePathEnforcesForeignKeysAndWaits(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, "sqlite", filepath.Join(t.TempDir(), "soft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck

	// Several connections at once, each set up by the driver.
	conns := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		c, err := d.Connx(ctx)
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

// Write-ahead logging is opt-in, as the README describes: an existing
// database switches to it when the data source asks, keeps its rows, keeps
// foreign keys and the busy timeout, and stays in WAL mode when the pragma is
// removed again.
func TestOpenSqliteWALOptIn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "soft.db")
	open := func(dsn string) *DB {
		t.Helper()
		d, err := Open(ctx, "sqlite", dsn)
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

	d := open(path)
	if m := mode(d); m != "delete" {
		t.Errorf("default journal mode = %q, want delete", m)
	}
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	d.Close() //nolint:errcheck

	d = open(path + "?_pragma=journal_mode(WAL)")
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
		t.Errorf("with WAL asked for: journal_mode %q, foreign_keys %d, busy_timeout %d, rows %d; want wal, 1, %d, 1",
			m, fk, busy, n, defaultSqliteBusyTimeout)
	}
	d.Close() //nolint:errcheck

	d = open(path)
	defer d.Close() //nolint:errcheck
	if m := mode(d); m != "wal" {
		t.Errorf("journal mode after the pragma is removed = %q, want wal", m)
	}
}
