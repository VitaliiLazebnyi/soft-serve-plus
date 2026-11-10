package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
)

const (
	createTablesName    = "create tables"
	createTablesVersion = 1
)

var createTables = Migration{
	Version: createTablesVersion,
	Name:    createTablesName,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		cfg := config.FromContext(ctx)
		if cfg == nil {
			return errNoConfig
		}

		// Seed rows are inserted only when missing, so the migration also
		// runs over tables left behind by an earlier rollback. SQLite says so
		// with INSERT OR IGNORE, Postgres with a trailing ON CONFLICT clause.
		insert := "INSERT "
		var onConflict string
		if tx.DriverName() == "postgres" {
			onConflict = " ON CONFLICT DO NOTHING"
		}

		// Alter old tables (if exist)
		// This is to support prior versions of Soft Serve v0.6
		switch tx.DriverName() {
		case "sqlite":
			insert += "OR IGNORE "

			if ok, err := tableExists(ctx, tx, "user"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE user RENAME TO user_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(ctx, tx, "public_key"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE public_key RENAME TO public_key_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(ctx, tx, "collab"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE collab RENAME TO collab_old"); err != nil {
					return err
				}
			}

			if ok, err := tableExists(ctx, tx, "repo"); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE repo RENAME TO repo_old"); err != nil {
					return err
				}
			}
		}

		if err := migrateUp(ctx, tx, createTablesVersion, createTablesName); err != nil {
			return err
		}

		// Import the v0.6 users first, keeping their ids, so the default
		// admin below does not take an id a legacy user has.
		sqlite := tx.DriverName() == "sqlite"
		if sqlite {
			if ok, err := tableExists(ctx, tx, "user_old"); err != nil {
				return err
			} else if ok {
				sqlm := `
				INSERT INTO users (id, username, admin, updated_at)
					SELECT id, username, admin, updated_at FROM user_old;
				`
				if _, err := tx.ExecContext(ctx, sqlm); err != nil {
					return err
				}
			}
		}

		// Insert default user. It goes in before the legacy repositories are
		// imported, since they are given to an admin.
		insertUser := tx.Rebind(insert + "INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)" + onConflict)
		if _, err := tx.ExecContext(ctx, insertUser, "admin", true); err != nil {
			return err
		}

		if sqlite {
			if err := importLegacyTables(ctx, tx); err != nil {
				return err
			}
		}

		// Seed the keys to the admin named "admin", not to whatever account
		// has id 1: with imported legacy users that may be anyone. When there
		// is no such admin, the keys are left out (with a warning) and stay
		// admin keys through the config.
		adminKeys := cfg.AdminKeys()
		var adminID int64
		if len(adminKeys) > 0 {
			var ok bool
			var err error
			adminID, ok, err = adminUserID(ctx, tx, log.FromContext(ctx).WithPrefix("migrate"))
			if err != nil {
				return err
			}
			if !ok {
				adminKeys = nil
			}
		}
		for _, k := range adminKeys {
			query := tx.Rebind(insert + "INTO public_keys (user_id, public_key, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)" + onConflict)
			ak := sshutils.MarshalAuthorizedKey(k)
			if _, err := tx.ExecContext(ctx, query, adminID, ak); err != nil {
				if errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
					continue
				}
				return err
			}
		}

		// Insert default settings
		insertSettings := insert + "INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)" + onConflict
		insertSettings = tx.Rebind(insertSettings)
		settings := []struct {
			Key   string
			Value string
		}{
			{"allow_keyless", "true"},
			{"anon_access", access.ReadOnlyAccess.String()},
			{"init", "true"},
		}

		for _, s := range settings {
			if _, err := tx.ExecContext(ctx, insertSettings, s.Key, s.Value); err != nil {
				return fmt.Errorf("inserting default settings %q: %w", s.Key, err)
			}
		}

		return nil
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, createTablesVersion, createTablesName)
	},
}

// importLegacyTables imports the Soft Serve v0.6 public keys, repositories
// and collaborators renamed to *_old, after the users.
//
// Foreign keys stay enforced: "PRAGMA foreign_keys" is a no-op inside a
// transaction. Rows that point at a user or repository that no longer
// exists are left out with a warning instead of failing the migration.
func importLegacyTables(ctx context.Context, tx *db.Tx) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")

	if ok, err := tableExists(ctx, tx, "public_key_old"); err != nil {
		return err
	} else if ok {
		// Check duplicate keys
		pks := []struct {
			ID        string `db:"id"`
			PublicKey string `db:"public_key"`
		}{}
		if err := tx.SelectContext(ctx, &pks, "SELECT id, public_key FROM public_key_old"); err != nil {
			return err
		}

		pkss := map[string]struct{}{}
		for _, pk := range pks {
			if _, ok := pkss[pk.PublicKey]; ok {
				return fmt.Errorf("duplicate public key: %q, please remove the duplicate key and try again", pk.PublicKey)
			}
			pkss[pk.PublicKey] = struct{}{}
		}

		sqlm := `
		INSERT INTO public_keys (id, user_id, public_key, created_at, updated_at)
			SELECT id, user_id, public_key, created_at, updated_at FROM public_key_old
			WHERE user_id IN (SELECT id FROM users);
		`
		if err := importLegacyRows(ctx, tx, logger, "public_key_old", sqlm); err != nil {
			return err
		}
	}

	if ok, err := tableExists(ctx, tx, "repo_old"); err != nil {
		return err
	} else if ok {
		var n int
		if err := tx.GetContext(ctx, &n, "SELECT COUNT(*) FROM repo_old"); err != nil {
			return err
		}
		var owner int64
		if n > 0 {
			// v0.6 repositories have no owner; give them to the first admin.
			if err := tx.GetContext(ctx, &owner, "SELECT id FROM users WHERE admin = true ORDER BY id LIMIT 1"); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("no admin user to own the imported repositories: user %q exists and is not an admin; make a user an admin and try again", defaultAdminUsername)
				}
				return err
			}
		}

		sqlm := `
		INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, created_at, updated_at, user_id)
			SELECT id, name, project_name, description, private, mirror, hidden, created_at, updated_at, ? FROM repo_old;
		`
		if _, err := tx.ExecContext(ctx, sqlm, owner); err != nil {
			return err
		}
	}

	if ok, err := tableExists(ctx, tx, "collab_old"); err != nil {
		return err
	} else if ok {
		sqlm := `
		INSERT INTO collabs (id, user_id, repo_id, access_level, created_at, updated_at)
			SELECT id, user_id, repo_id, ` + strconv.Itoa(int(access.ReadWriteAccess)) + `, created_at, updated_at FROM collab_old
			WHERE user_id IN (SELECT id FROM users) AND repo_id IN (SELECT id FROM repos);
		`
		if err := importLegacyRows(ctx, tx, logger, "collab_old", sqlm); err != nil {
			return err
		}
	}

	return nil
}

// importLegacyRows runs an INSERT ... SELECT from the legacy table and warns
// about the rows it left out.
func importLegacyRows(ctx context.Context, tx *db.Tx, logger *log.Logger, table, query string) error {
	var total int64
	if err := tx.GetContext(ctx, &total, "SELECT COUNT(*) FROM "+table); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, query)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if skipped := total - n; skipped > 0 {
		logger.Warn("left out legacy rows that refer to a missing user or repository", "table", table, "rows", skipped)
	}
	return nil
}
