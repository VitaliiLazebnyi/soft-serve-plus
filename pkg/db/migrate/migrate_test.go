package migrate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

func TestMigrate(t *testing.T) {
	// XXX: we need a config.Config in the context for the migrations to run
	// properly. Some migrations depend on the config being present.
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Errorf("Migrate() => %v, want nil error", err)
	}
}

// Servers starting at the same time on a shared Postgres database must not
// run the same migrations at once: all but one would fail on duplicate
// objects. SQLite write transactions are already serialised by BEGIN
// IMMEDIATE, but the test database has no busy_timeout to wait with.
func TestMigrateConcurrent(t *testing.T) {
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
			testMigrateConcurrent(t, isolation)
		})
	}
}

func testMigrateConcurrent(t *testing.T, isolation string) {
	t.Helper()
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if isolation != "" {
		setDefaultIsolation(ctx, t, dbx, isolation)
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
			errs[i] = Migrate(ctx, dbx)
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

// setDefaultIsolation makes isolation the default transaction isolation level
// of the Postgres database dbx is connected to, for every connection dbx
// opens from now on.
func setDefaultIsolation(ctx context.Context, t *testing.T, dbx *db.DB, isolation string) {
	t.Helper()
	var name string
	if err := dbx.GetContext(ctx, &name, "SELECT current_database()"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbx.ExecContext(ctx, fmt.Sprintf("ALTER DATABASE %q SET default_transaction_isolation = '%s'", name, isolation)); err != nil {
		t.Fatal(err)
	}
	// The setting applies to new sessions only: drop the pooled ones.
	dbx.SetMaxIdleConns(0)
	dbx.SetMaxIdleConns(2)

	var got string
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		return tx.GetContext(ctx, &got, "SHOW transaction_isolation")
	}); err != nil {
		t.Fatal(err)
	}
	if got != isolation {
		// Options in SOFT_SERVE_DB_DATA_SOURCE take precedence over the
		// database default.
		t.Skipf("the connection overrides the database default isolation level: got %q, want %q", got, isolation)
	}
}

// An older binary must refuse to run against a database that a newer binary
// has already migrated past the newest migration it knows.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:len(all)-1]

	err = Migrate(ctx, dbx)
	if err == nil {
		t.Fatal("Migrate() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Migrate() on a newer database => %v, want a version mismatch error", err)
	}

	// The binary that knows every migration still starts.
	migrations = all
	if err := Migrate(ctx, dbx); err != nil {
		t.Errorf("Migrate() with all migrations => %v, want nil error", err)
	}
}

// Rolling back a database that a newer binary migrated past the newest
// migration this binary knows must say so, and must leave the database alone.
func TestRollbackRefusesNewerDatabase(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:len(all)-1]

	err = Rollback(ctx, dbx)
	if err == nil {
		t.Fatal("Rollback() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Rollback() on a newer database => %v, want a version mismatch error", err)
	}

	var version int64
	if err := dbx.Get(&version, "SELECT MAX(version) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if want := all[len(all)-1].Version; version != want {
		t.Errorf("migration version after refused Rollback() = %d, want %d", version, want)
	}
}

// Rollback must pick the migration by its version, not by its position in
// the list, so a gap in version numbers rolls back the right migration.
func TestRollbackByVersion(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	all := migrations
	t.Cleanup(func() { migrations = all })

	var rolledBack []string
	mk := func(version int64, name string) Migration {
		return Migration{
			Version: version,
			Name:    name,
			Migrate: func(context.Context, *db.Tx) error { return nil },
			Rollback: func(context.Context, *db.Tx) error {
				rolledBack = append(rolledBack, name)
				return nil
			},
		}
	}
	migrations = []Migration{mk(1, "one"), mk(3, "three")}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}
	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v, want nil error", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != "three" {
		t.Errorf("Rollback() rolled back %v, want [three]", rolledBack)
	}

	// A recorded version this binary has no migration for is an error, not a
	// rollback of some other migration.
	if _, err := dbx.Exec(dbx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), "two", 2); err != nil {
		t.Fatal(err)
	}
	rolledBack = nil
	if err := Rollback(ctx, dbx); err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
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
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	err = Rollback(ctx, dbx)
	if err == nil || err.Error() != "there are no migrations to rollback" {
		t.Errorf("Rollback() without a migrations table => %v, want %q", err, "there are no migrations to rollback")
	}

	// A migrations table the query cannot read: it has no version column.
	if _, err := dbx.Exec("CREATE TABLE migrations (id INTEGER, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	err = Rollback(ctx, dbx)
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
