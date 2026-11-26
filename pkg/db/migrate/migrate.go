// Package migrate creates, upgrades and rolls back the database schema, and
// keeps the admin keys in the database in step with initial_admin_keys.
package migrate

import (
	"context"
	"errors"
	"fmt"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// Env is what a migration needs besides its transaction.
type Env struct {
	// Config is the server configuration. It is never nil.
	Config *config.Config
	// Logger logs with the "migrate" prefix. It is never nil.
	Logger *log.Logger
}

// Func is a function that executes a migration.
type Func func(ctx context.Context, tx *db.Tx, env Env) error

// Migration is a struct that contains the name of the migration and the
// function to execute it.
type Migration struct {
	Version  int64
	Name     string
	Migrate  Func
	Rollback Func
}

// Migrations is a database model to store migrations.
type Migrations struct {
	ID      int64  `db:"id"`
	Name    string `db:"name"`
	Version int64  `db:"version"`
}

// schema returns the statement that creates the migrations table, for
// sqlite or postgres, the drivers db.Open accepts.
func (Migrations) schema(driverName string) string {
	if driverName == "postgres" {
		return `CREATE TABLE IF NOT EXISTS migrations (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			version INTEGER NOT NULL UNIQUE
		);
	`
	}
	return `CREATE TABLE IF NOT EXISTS migrations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			version INTEGER NOT NULL UNIQUE
		);
	`
}

// newEnv returns the Env of cfg and logger. A nil logger is the default
// logger.
func newEnv(cfg *config.Config, logger *log.Logger) (Env, error) {
	if cfg == nil {
		return Env{}, config.ErrNilConfig
	}
	if logger == nil {
		logger = log.Default()
	}
	return Env{Config: cfg, Logger: logger.WithPrefix("migrate")}, nil
}

// Migrate runs the migrations with the server configuration cfg, logging to
// logger (the default logger when nil).
func Migrate(ctx context.Context, dbx *db.DB, cfg *config.Config, logger *log.Logger) error {
	env, err := newEnv(cfg, logger)
	if err != nil {
		return err
	}
	return migrate(ctx, dbx, env, migrations)
}

// migrate runs the migrations of list, which is in order of execution.
func migrate(ctx context.Context, dbx *db.DB, env Env, list []Migration) error {
	logger := env.Logger
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := lockMigrations(ctx, tx); err != nil {
			return err
		}

		ok, err := tableExists(ctx, tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, Migrations{}.schema(tx.DriverName())); err != nil {
				return err
			}
		}

		current, err := checkApplied(ctx, tx, list)
		if err != nil {
			return err
		}

		pending := len(list) > 0 && list[len(list)-1].Version > current
		if err := removeSqliteOrphans(ctx, tx, logger, pending); err != nil {
			return err
		}

		for _, m := range list {
			if m.Version <= current {
				continue
			}

			logger.Infof("running migration %d. %s", m.Version, m.Name)
			if err := m.Migrate(ctx, tx, env); err != nil {
				return fmt.Errorf("migration %d %s: %w", m.Version, m.Name, err)
			}

			if _, err := tx.ExecContext(ctx, tx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), m.Name, m.Version); err != nil {
				return err
			}
		}

		return nil
	})
}

// Rollback rolls back the latest migration, with the server configuration
// cfg, logging to logger (the default logger when nil).
func Rollback(ctx context.Context, dbx *db.DB, cfg *config.Config, logger *log.Logger) error {
	env, err := newEnv(cfg, logger)
	if err != nil {
		return err
	}
	return rollback(ctx, dbx, env, migrations)
}

// rollback rolls back the latest migration of list.
func rollback(ctx context.Context, dbx *db.DB, env Env, list []Migration) error {
	logger := env.Logger
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

		current, err := checkApplied(ctx, tx, list)
		if err != nil {
			return err
		}

		if current == 0 {
			return fmt.Errorf("there are no migrations to rollback")
		}

		// checkApplied has made sure this binary knows every applied
		// migration, so the latest one is found.
		var m *Migration
		for i := range list {
			if list[i].Version == current {
				m = &list[i]
				break
			}
		}
		if m == nil {
			return fmt.Errorf("database is at migration version %d, which is unknown to this binary", current)
		}

		logger.Infof("rolling back migration %d. %s", m.Version, m.Name)
		if err := m.Rollback(ctx, tx, env); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, tx.Rebind("DELETE FROM migrations WHERE version = ?"), current); err != nil {
			return err
		}

		return nil
	})
}

// checkApplied checks every migration recorded in the database against the
// migrations of list, the ones this binary knows, and returns the latest
// applied version, 0 when
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
func checkApplied(ctx context.Context, tx *db.Tx, list []Migration) (int64, error) {
	var applied []Migrations
	if err := tx.SelectContext(ctx, &applied, tx.Rebind("SELECT id, name, version FROM migrations ORDER BY version ASC")); err != nil {
		return 0, fmt.Errorf("list applied migrations: %w", err)
	}
	if len(applied) == 0 {
		return 0, nil
	}

	current := applied[len(applied)-1].Version
	if n := len(list); n == 0 || current > list[n-1].Version {
		var latest int64
		if n > 0 {
			latest = list[n-1].Version
		}
		return 0, fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", current, latest)
	}

	known := make(map[int64]string, len(list))
	for _, m := range list {
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
	for _, m := range list {
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
// A run that waited must see what the previous holder committed. It does:
// transactions run in READ COMMITTED on Postgres (see db.DB.transaction), so
// the statements after the lock take a new snapshot.
//
// SQLite write transactions are already serialised: they start with BEGIN
// IMMEDIATE (see db.Open).
func lockMigrations(ctx context.Context, tx *db.Tx) error {
	if tx.DriverName() != "postgres" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	return nil
}

// tableExists reports whether the table exists, returning any error from the
// lookup other than the table not being found.
func tableExists(ctx context.Context, tx *db.Tx, tableName string) (bool, error) {
	query := "SELECT name FROM sqlite_master WHERE type='table' AND name=?"
	if tx.DriverName() == "postgres" {
		// Look in the schema unqualified CREATE TABLE statements write to,
		// which follows search_path and is not necessarily "public".
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	}

	query = tx.Rebind(query)
	var name string
	if err := tx.GetContext(ctx, &name, query, tableName); err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
