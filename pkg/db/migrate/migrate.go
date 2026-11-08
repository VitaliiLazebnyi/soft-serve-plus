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

func (Migrations) schema(driverName string) string {
	switch driverName {
	case "sqlite3", "sqlite":
		return `CREATE TABLE IF NOT EXISTS migrations (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				version INTEGER NOT NULL UNIQUE
			);
		`
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS migrations (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			version INTEGER NOT NULL UNIQUE
		);
	`
	case "mysql":
		return `CREATE TABLE IF NOT EXISTS migrations (
			id INT NOT NULL AUTO_INCREMENT,
			name TEXT NOT NULL,
			version INT NOT NULL,
			UNIQUE (version),
			PRIMARY KEY (id)
		);
	`
	default:
		panic("unknown driver")
	}
}

// Migrate runs the migrations.
func Migrate(ctx context.Context, dbx *db.DB) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := lockMigrations(ctx, tx); err != nil {
			return err
		}

		ok, err := tableExists(tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, Migrations{}.schema(tx.DriverName())); err != nil {
				return err
			}
		}

		current, err := checkApplied(ctx, tx)
		if err != nil {
			return err
		}

		for _, m := range migrations {
			if m.Version <= current {
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

		ok, err := tableExists(tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			return fmt.Errorf("there are no migrations to rollback")
		}

		current, err := checkApplied(ctx, tx)
		if err != nil {
			return err
		}

		if current == 0 {
			return fmt.Errorf("there are no migrations to rollback")
		}

		// checkApplied has made sure this binary knows every applied
		// migration, so the latest one is found.
		var m *Migration
		for i := range migrations {
			if migrations[i].Version == current {
				m = &migrations[i]
				break
			}
		}
		if m == nil {
			return fmt.Errorf("database is at migration version %d, which is unknown to this binary", current)
		}

		logger.Infof("rolling back migration %d. %s", m.Version, m.Name)
		if err := m.Rollback(ctx, tx); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, tx.Rebind("DELETE FROM migrations WHERE version = ?"), current); err != nil {
			return err
		}

		return nil
	})
}

// checkApplied checks every migration recorded in the database against the
// migrations this binary knows and returns the latest applied version, 0 when
// none is. It refuses a database that:
//
//   - a newer binary migrated past the latest migration known here, since its
//     schema may not match what this binary expects;
//   - records a migration under a version this binary knows by another name,
//     or one this binary does not know at all, such as a database migrated by
//     a build whose migration numbers collide with this one's;
//   - lacks a migration that is older than the latest applied one, which
//     would otherwise never run.
//
// Comparing only the latest version would miss all but the first case.
func checkApplied(ctx context.Context, tx *db.Tx) (int64, error) {
	var applied []Migrations
	if err := tx.SelectContext(ctx, &applied, tx.Rebind("SELECT * FROM migrations ORDER BY version ASC")); err != nil {
		return 0, fmt.Errorf("list applied migrations: %w", err)
	}
	if len(applied) == 0 {
		return 0, nil
	}

	current := applied[len(applied)-1].Version
	if n := len(migrations); n == 0 || current > migrations[n-1].Version {
		var latest int64
		if n > 0 {
			latest = migrations[n-1].Version
		}
		return 0, fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", current, latest)
	}

	known := make(map[int64]string, len(migrations))
	for _, m := range migrations {
		known[m.Version] = m.Name
	}
	done := make(map[int64]bool, len(applied))
	for _, a := range applied {
		name, ok := known[a.Version]
		if !ok {
			return 0, fmt.Errorf("database has migration %d %q, which is unknown to this binary; it was migrated by a different soft-serve build", a.Version, a.Name)
		}
		if a.Name != name {
			return 0, fmt.Errorf("database has migration %d %q, but this binary's migration %d is %q; it was migrated by a different soft-serve build", a.Version, a.Name, a.Version, name)
		}
		done[a.Version] = true
	}
	for _, m := range migrations {
		if m.Version > current {
			break
		}
		if !done[m.Version] {
			return 0, fmt.Errorf("database is missing migration %d %q although the later migration %d is applied; it was migrated by a different soft-serve build", m.Version, m.Name, current)
		}
	}

	return current, nil
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
func tableExists(tx *db.Tx, tableName string) (bool, error) {
	var query string
	switch tx.DriverName() {
	case "sqlite3", "sqlite":
		query = "SELECT name FROM sqlite_master WHERE type='table' AND name=?"
	case "postgres":
		// Look in the schema unqualified CREATE TABLE statements write to,
		// which follows search_path and is not necessarily "public".
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	case "mysql":
		// A MySQL schema is a database; look in the connection's current one.
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	}

	query = tx.Rebind(query)
	var name string
	if err := tx.Get(&name, query, tableName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
