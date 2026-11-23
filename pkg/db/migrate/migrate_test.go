package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/jmoiron/sqlx"
)

// testEnv returns the Env migrations get from Migrate with cfg and the
// default logger.
func testEnv(cfg *config.Config) Env {
	env, err := newEnv(cfg, nil)
	if err != nil {
		panic(err)
	}
	return env
}

func TestMigrate(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Errorf("Migrate() => %v, want nil error", err)
	}
}

// Servers starting at the same time on a shared Postgres database must not
// run the same migrations at once: all but one would fail on duplicate
// objects. SQLite write transactions are already serialised by BEGIN
// IMMEDIATE, but the test database has no busy_timeout to wait with.
func TestMigrateConcurrent(t *testing.T) {
	t.Parallel()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("needs Postgres (SOFT_SERVE_DB_DRIVER=postgres)")
	}
	// The database default isolation level must not matter: under
	// REPEATABLE READ or SERIALIZABLE a run that waited for the lock would
	// otherwise read the schema version from before it waited.
	for _, isolation := range []string{"", "repeatable read", "serializable"} {
		name := isolation
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testMigrateConcurrent(t, isolation)
		})
	}
}

func testMigrateConcurrent(t *testing.T, isolation string) {
	t.Helper()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if isolation != "" {
		dbtest.SetDefaultIsolation(ctx, t, dbx, isolation)
	}

	const n = 4
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = migrate(ctx, dbx, env, migrations)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate() #%d => %v, want nil error", i, err)
		}
	}

	var count, version int64
	if err := dbx.GetContext(ctx, &count, "SELECT COUNT(*) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if err := dbx.GetContext(ctx, &version, "SELECT MAX(version) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if want := int64(len(migrations)); count != want {
		t.Errorf("migrations recorded = %d, want %d", count, want)
	}
	if want := migrations[len(migrations)-1].Version; version != want {
		t.Errorf("migration version = %d, want %d", version, want)
	}
}

// An older binary must refuse to run against a database that a newer binary
// has already migrated past the newest migration it knows.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	list := all[:len(all)-1]

	err = migrate(ctx, dbx, env, list)
	if err == nil {
		t.Fatal("Migrate() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Migrate() on a newer database => %v, want a version mismatch error", err)
	}

	// The binary that knows every migration still starts.
	list = all
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Errorf("Migrate() with all migrations => %v, want nil error", err)
	}
}

// Rolling back a database that a newer binary migrated past the newest
// migration this binary knows must say so, and must leave the database alone.
func TestRollbackRefusesNewerDatabase(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	list := all[:len(all)-1]

	err = rollback(ctx, dbx, env, list)
	if err == nil {
		t.Fatal("Rollback() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Rollback() on a newer database => %v, want a version mismatch error", err)
	}

	var version int64
	if err := dbx.GetContext(context.Background(), &version, "SELECT MAX(version) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if want := all[len(all)-1].Version; version != want {
		t.Errorf("migration version after refused Rollback() = %d, want %d", version, want)
	}
}

// Rollback must pick the migration by its version, not by its position in
// the list, so a gap in version numbers rolls back the right migration.
func TestRollbackByVersion(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	var rolledBack []string
	mk := func(version int64, name string) Migration {
		return Migration{
			Version: version,
			Name:    name,
			Migrate: func(context.Context, *db.Tx, Env) error { return nil },
			Rollback: func(context.Context, *db.Tx, Env) error {
				rolledBack = append(rolledBack, name)
				return nil
			},
		}
	}
	list := []Migration{mk(1, "one"), mk(3, "three")}

	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}
	if err := rollback(ctx, dbx, env, list); err != nil {
		t.Fatalf("Rollback() => %v, want nil error", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != "three" {
		t.Errorf("Rollback() rolled back %v, want [three]", rolledBack)
	}

	// A recorded version this binary has no migration for is an error, not a
	// rollback of some other migration.
	if _, err := dbx.ExecContext(ctx, dbx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), "two", 2); err != nil {
		t.Fatal(err)
	}
	rolledBack = nil
	if err := rollback(ctx, dbx, env, list); err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Errorf("Rollback() at unknown version 2 => %v, want an unknown version error", err)
	}
	if len(rolledBack) != 0 {
		t.Errorf("Rollback() at unknown version 2 rolled back %v, want nothing", rolledBack)
	}
}

// Rollback must report a failure to read the migrations table as that
// failure, not as "there are no migrations to rollback". A database that has
// no migrations table at all still has nothing to roll back.
func TestRollbackReportsQueryError(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	err = rollback(ctx, dbx, env, migrations)
	if err == nil || err.Error() != "there are no migrations to rollback" {
		t.Errorf("Rollback() without a migrations table => %v, want %q", err, "there are no migrations to rollback")
	}

	// A migrations table the query cannot read: it has no version column.
	if _, err := dbx.ExecContext(ctx, "CREATE TABLE migrations (id INTEGER, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	err = rollback(ctx, dbx, env, migrations)
	if err == nil {
		t.Fatal("Rollback() with an unreadable migrations table => nil error, want an error")
	}
	if strings.Contains(err.Error(), "no migrations to rollback") {
		t.Errorf("Rollback() with an unreadable migrations table => %v, want the query error", err)
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("Rollback() with an unreadable migrations table => %v, want an error about the version column", err)
	}
}

// Rolling back every migration must leave none of their tables behind, and
// migrating again must then succeed.
func TestRollbackAllThenMigrate(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	for range migrations {
		if err := rollback(ctx, dbx, env, migrations); err != nil {
			t.Fatalf("Rollback() => %v, want nil error", err)
		}
	}
	if err := rollback(ctx, dbx, env, migrations); err == nil || err.Error() != "there are no migrations to rollback" {
		t.Errorf("Rollback() after rolling back everything => %v, want %q", err, "there are no migrations to rollback")
	}

	tables := []string{
		"settings", "users", "public_keys", "repos", "collabs",
		"lfs_objects", "lfs_locks", "access_tokens",
		"webhooks", "webhook_events", "webhook_deliveries",
	}
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		for _, table := range tables {
			ok, err := tableExists(ctx, tx, table)
			if err != nil {
				return err
			}
			if ok {
				t.Errorf("table %q exists after rolling back every migration", table)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() after rolling back every migration => %v, want nil error", err)
	}
	var admins int
	if err := dbx.GetContext(context.Background(), &admins, dbx.Rebind("SELECT COUNT(*) FROM users WHERE username = ?"), "admin"); err != nil {
		t.Fatal(err)
	}
	if admins != 1 {
		t.Errorf("admin users after migrating again = %d, want 1", admins)
	}
}

// The first migrations must also run over their tables when they are still
// there, as an older binary left them: its rollbacks of migrations 1-3
// dropped nothing.
func TestMigrateOverLeftoverTables(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Roll back down to migration 3, then forget migrations 1-3 but keep
	// their tables and seed rows.
	for _, m := range migrations {
		if m.Version <= migrateLfsObjectsVersion {
			continue
		}
		if err := rollback(ctx, dbx, env, migrations); err != nil {
			t.Fatalf("Rollback() => %v, want nil error", err)
		}
	}
	if _, err := dbx.ExecContext(ctx, "DELETE FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() over leftover tables => %v, want nil error", err)
	}

	var settings int
	// The orphan check's marker, on SQLite, is not a seed row.
	if err := dbx.GetContext(context.Background(), &settings, dbx.Rebind("SELECT COUNT(*) FROM settings WHERE \"key\" <> ?"), orphansCheckedKey); err != nil {
		t.Fatal(err)
	}
	if settings != 4 {
		t.Errorf("settings after migrating over leftover tables = %d, want 4", settings)
	}
}

// tableExists looks for a table in the schema that unqualified statements use.
// On Postgres that is the first schema on search_path, which need not be
// "public". The test runs on Postgres when SOFT_SERVE_DB_DRIVER is
// "postgres".
func TestHasTable(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		hasTable := func(name string) bool {
			t.Helper()
			ok, err := tableExists(ctx, tx, name)
			if err != nil {
				t.Fatalf("tableExists(%s): %v", name, err)
			}
			return ok
		}

		if dbx.DriverName() == "postgres" {
			for _, q := range []string{
				`CREATE TABLE public.only_in_public (id INTEGER)`,
				`CREATE SCHEMA app`,
				`SET LOCAL search_path TO app`,
			} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if hasTable("only_in_public") {
				t.Errorf("tableExists(only_in_public) = true with search_path=app, want false")
			}
		}

		if hasTable("widgets") {
			t.Errorf("tableExists(widgets) before CREATE = true, want false")
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE widgets (id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if !hasTable("widgets") {
			t.Errorf("tableExists(widgets) after CREATE = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Rollback must return a failure of its migrations table lookup, such as a
// lost connection, instead of reading it as "no migrations table".
func TestRollbackReportsTableLookupError(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New("connection lost")
	conn := &recordingConn{err: lookupErr}
	dbx := db.Wrap(sqlx.NewDb(sql.OpenDB(recordingConnector{conn}), "sqlite"), db.Options{})
	t.Cleanup(func() { _ = dbx.Close() })

	err := Rollback(context.TODO(), dbx, config.DefaultConfig(), nil)
	if !errors.Is(err, lookupErr) {
		t.Errorf("Rollback() with a failing table lookup => %v, want it to wrap %v", err, lookupErr)
	}
}

// Migrate must return a failure of its migrations table lookup instead of
// reading it as "no migrations table" and going on to create one.
func TestMigrateReportsTableLookupError(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New("connection lost")
	conn := &recordingConn{err: lookupErr}
	dbx := db.Wrap(sqlx.NewDb(sql.OpenDB(recordingConnector{conn}), "sqlite"), db.Options{})
	t.Cleanup(func() { _ = dbx.Close() })

	err := Migrate(context.TODO(), dbx, config.DefaultConfig(), nil)
	if !errors.Is(err, lookupErr) {
		t.Errorf("Migrate() with a failing table lookup => %v, want it to wrap %v", err, lookupErr)
	}
}

// The first migration must return a failure of its legacy table lookups
// instead of reading it as "no legacy table" and skipping the import.
func TestCreateTablesReportsTableLookupError(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New("connection lost")
	conn := &recordingConn{err: lookupErr}
	dbx := db.Wrap(sqlx.NewDb(sql.OpenDB(recordingConnector{conn}), "sqlite"), db.Options{})
	t.Cleanup(func() { _ = dbx.Close() })

	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		return createTables.Migrate(ctx, tx, env)
	})
	if !errors.Is(err, lookupErr) {
		t.Errorf("createTables.Migrate() with a failing table lookup => %v, want it to wrap %v", err, lookupErr)
	}
}

type recordingConnector struct{ conn *recordingConn }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c recordingConnector) Driver() driver.Driver                        { return nil }

// recordingConn records each query and returns no rows, or err if set.
type recordingConn struct {
	queries []string
	args    []driver.Value
	err     error
}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *recordingConn) Close() error              { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) { return recordingTx{}, nil }

func (c *recordingConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	c.args = append(c.args, args...)
	if c.err != nil {
		return nil, c.err
	}
	return emptyRows{}, nil
}

type recordingTx struct{}

func (recordingTx) Commit() error   { return nil }
func (recordingTx) Rollback() error { return nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"table_name"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }
