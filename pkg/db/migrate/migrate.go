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
