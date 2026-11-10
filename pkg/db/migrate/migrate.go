package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// MigrateFunc is a function that executes a migration.
type MigrateFunc func(ctx context.Context, tx *db.Tx) error //nolint:revive

// Migration is a struct that contains the name of the migration and the
// function to execute it.
type Migration struct {
	Version  int64
	Name     string
	Migrate  MigrateFunc
	Rollback MigrateFunc
}

// Migrations is a database model to store migrations.
type Migrations struct {
	ID      int64  `db:"id"`
	Name    string `db:"name"`
	Version int64  `db:"version"`
}

// schema returns the statement that creates the migrations table. Only the
// drivers db.Open links in are supported.
func (Migrations) schema(driverName string) (string, error) {
	switch driverName {
	case "sqlite":
		return `CREATE TABLE IF NOT EXISTS migrations (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				version INTEGER NOT NULL UNIQUE
			);
		`, nil
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS migrations (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			version INTEGER NOT NULL UNIQUE
		);
	`, nil
	default:
		return "", fmt.Errorf("migrate: unsupported database driver %q", driverName)
	}
}

// errNoConfig is returned by migrations that need the server configuration
// when ctx carries none.
var errNoConfig = errors.New("migrate: no config in context")

// Migrate runs the migrations. ctx must carry the server configuration
// (config.WithContext).
func Migrate(ctx context.Context, dbx *db.DB) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := lockMigrations(ctx, tx); err != nil {
			return err
		}

		ok, err := tableExists(ctx, tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			schema, err := Migrations{}.schema(tx.DriverName())
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return err
			}
		}

		var migrs Migrations
		if err := tx.GetContext(ctx, &migrs, tx.Rebind("SELECT * FROM migrations ORDER BY version DESC LIMIT 1")); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		// Refuse to run against a database migrated by a newer binary: its
		// schema may not match what this binary expects.
		if n := len(migrations); n > 0 && migrs.Version > migrations[n-1].Version {
			return fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", migrs.Version, migrations[n-1].Version)
		}

		for _, m := range migrations {
			if m.Version <= migrs.Version {
				continue
			}

			logger.Infof("running migration %d. %s", m.Version, m.Name)
			if err := m.Migrate(ctx, tx); err != nil {
				return err
			}

			if _, err := tx.ExecContext(ctx, tx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), m.Name, m.Version); err != nil {
				return err
			}
		}

		return nil
	})
}

// Rollback rolls back a migration.
func Rollback(ctx context.Context, dbx *db.DB) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := lockMigrations(ctx, tx); err != nil {
			return err
		}

		ok, err := tableExists(ctx, tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			return fmt.Errorf("there are no migrations to rollback")
		}

		var migrs Migrations
		if err := tx.GetContext(ctx, &migrs, tx.Rebind("SELECT * FROM migrations ORDER BY version DESC LIMIT 1")); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("get latest migration: %w", err)
			}
		}

		if migrs.Version == 0 {
			return fmt.Errorf("there are no migrations to rollback")
		}

		// Look the migration up by version rather than by position, and
		// refuse to roll back one this binary does not know.
		var m *Migration
		for i := range migrations {
			if migrations[i].Version == migrs.Version {
				m = &migrations[i]
				break
			}
		}
		if m == nil {
			if n := len(migrations); n > 0 && migrs.Version > migrations[n-1].Version {
				return fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", migrs.Version, migrations[n-1].Version)
			}
			return fmt.Errorf("database is at migration version %d, which is unknown to this binary", migrs.Version)
		}

		logger.Infof("rolling back migration %d. %s", m.Version, m.Name)
		if err := m.Rollback(ctx, tx); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, tx.Rebind("DELETE FROM migrations WHERE version = ?"), migrs.Version); err != nil {
			return err
		}

		return nil
	})
}

// migrationLockID is the Postgres advisory lock key that serialises Migrate
// and Rollback. Advisory locks are scoped to the database, so servers sharing
// a Postgres server but using different databases do not wait on each other.
const migrationLockID int64 = 0x736f667473657276 // "softserv"

// lockMigrations makes concurrent Migrate and Rollback calls on the same
// database run one after another. Without it, servers starting at the same
// time on a shared Postgres database each see the same schema version and run
// the same migrations, and all but one fail on duplicate objects. The lock is
// held until tx ends. It must be the first statement in tx.
//
// A run that waited must see what the previous holder committed. Under READ
// COMMITTED each statement takes a new snapshot, so the statements after the
// lock see it. Under REPEATABLE READ or SERIALIZABLE the snapshot is taken
// when the lock statement starts, before it waits, so the run would read the
// old schema version and apply every migration again. The server, database,
// role or DSN can make either of those the default, so tx is set to READ
// COMMITTED explicitly before taking the lock.
//
// SQLite write transactions are already serialised: they start with BEGIN
// IMMEDIATE (see db.Open).
func lockMigrations(ctx context.Context, tx *db.Tx) error {
	if tx.DriverName() != "postgres" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return fmt.Errorf("set migration isolation level: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	return nil
}

// tableExists reports whether the table exists, returning any error from the
// lookup other than the table not being found.
func tableExists(ctx context.Context, tx *db.Tx, tableName string) (bool, error) {
	var query string
	switch driver := tx.DriverName(); driver {
	case "sqlite":
		query = "SELECT name FROM sqlite_master WHERE type='table' AND name=?"
	case "postgres":
		// Look in the schema unqualified CREATE TABLE statements write to,
		// which follows search_path and is not necessarily "public".
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	default:
		return false, fmt.Errorf("migrate: unsupported database driver %q", driver)
	}

	query = tx.Rebind(query)
	var name string
	if err := tx.GetContext(ctx, &name, query, tableName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
