package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

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
	} {
		if _, err := sqliteDSN(dsn); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("sqliteDSN(%q) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
		if _, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), dsn), Options{}); !errors.Is(err, errSqliteForeignKeysOff) {
			t.Errorf("Open(%q, Options{}) error = %v, want %v", dsn, err, errSqliteForeignKeysOff)
		}
	}
}

// A data source that is a bare path, as SOFT_SERVE_DB_DATA_SOURCE often is,
// still gets foreign keys and a busy timeout on every connection. Both used
// to be off: deleting a user left their keys behind, and any statement that
// met a git hook's write failed at once with "database is locked".
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

// A journal mode set with the driver's own _journal_mode or _journal key is
// the data source's mode, as one set with a _pragma is. The driver applies
// those keys after the _pragma list, so a journal_mode(WAL) added next to
// them made every new connection switch the database to WAL and back. A
// switch needs the database to itself: a connection opened while another
// held a read transaction failed with "database is locked".
func TestOpenSqliteJournalModeKeys(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"_journal_mode", "_journal"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "soft.db")
			// A short busy timeout: the failure this checks for waits it out.
			d, err := Open(ctx, "sqlite", path+"?_pragma=busy_timeout(100)&"+key+"=DELETE", Options{})
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
