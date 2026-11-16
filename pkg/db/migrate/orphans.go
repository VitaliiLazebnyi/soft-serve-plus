package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// orphansCheckedKey names the settings row that says a SQLite database was
// found free of rows that refer to missing rows.
const orphansCheckedKey = "foreign_keys_checked"

// maxOrphanPasses bounds the passes of removeSqliteOrphans. A pass can leave
// orphans behind only through ON DELETE CASCADE removing more rows, which
// foreign keys enforced does not leave dangling, so a second pass finds
// nothing; the bound is a guard, not an expected depth.
const maxOrphanPasses = 10

// fkViolation is a row of PRAGMA foreign_key_check.
type fkViolation struct {
	Table  string        `db:"table"`
	RowID  sql.NullInt64 `db:"rowid"`
	Parent string        `db:"parent"`
	FKID   int64         `db:"fkid"`
}

// removeSqliteOrphans removes the rows of a SQLite database that refer to a
// row that does not exist, once, before any pending migration runs.
//
// Foreign keys used to be enforced only by the default data source, so a
// database used with a data source that left them off can hold such rows:
// the keys and tokens of a deleted user, the collaborators, webhooks, LFS
// objects and locks of a deleted repository, the deliveries of a deleted
// webhook. With foreign keys now always enforced, they made the first
// migration that copies a table fail, and an orphaned key could not be added
// to another user. A repository whose owner was deleted is still a
// repository, with a directory, so it is given to the first admin instead of
// deleted. Each table's count is logged.
//
// A database found clean is marked in settings, so later starts skip the
// check, which reads every row that refers to another; with foreign keys
// enforced no new orphans appear.
func removeSqliteOrphans(ctx context.Context, tx *db.Tx, logger *log.Logger) error {
	if tx.DriverName() != "sqlite" {
		return nil
	}
	hasSettings, err := tableExists(ctx, tx, "settings")
	if err != nil {
		return fmt.Errorf("look up settings table: %w", err)
	}
	if hasSettings {
		var n int
		if err := tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM settings WHERE "key" = ?`, orphansCheckedKey); err != nil {
			return fmt.Errorf("look up the orphan check: %w", err)
		}
		if n > 0 {
			return nil
		}
	}

	deleted := map[string]int{}
	reassigned := 0
	for pass := 0; ; pass++ {
		var rows []fkViolation
		if err := tx.SelectContext(ctx, &rows, "PRAGMA foreign_key_check"); err != nil {
			return fmt.Errorf("check foreign keys: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if pass == maxOrphanPasses {
			return fmt.Errorf("rows referring to missing rows remain after %d passes, in %s", pass, rows[0].Table)
		}
		for _, r := range rows {
			if !r.RowID.Valid {
				return fmt.Errorf("table %s has a row referring to a missing %s row, and no rowid to remove it by", r.Table, r.Parent)
			}
			if r.Table == "repos" && r.Parent == "users" {
				ok, err := reassignRepo(ctx, tx, r.RowID.Int64)
				if err != nil {
					return err
				}
				if ok {
					reassigned++
					continue
				}
			}
			table := `"` + strings.ReplaceAll(r.Table, `"`, `""`) + `"`
			res, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rowid = ?", r.RowID.Int64)
			if err != nil {
				return fmt.Errorf("remove a %s row referring to a missing %s row: %w", r.Table, r.Parent, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				deleted[r.Table] += int(n)
			}
		}
	}

	for _, t := range slices.Sorted(maps.Keys(deleted)) {
		logger.Warn("removed rows that referred to rows that no longer exist, with the rows that referred to them", "table", t, "rows", deleted[t])
	}
	if reassigned > 0 {
		logger.Warn("gave repositories whose owner no longer exists to the first admin", "repos", reassigned)
	}
	if hasSettings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings ("key", value, updated_at) VALUES (?, 'true', CURRENT_TIMESTAMP)
			ON CONFLICT ("key") DO NOTHING`, orphansCheckedKey); err != nil {
			return fmt.Errorf("record the orphan check: %w", err)
		}
	}
	return nil
}

// reassignRepo gives the repository with rowid to the first admin. ok is
// false when there is no admin.
func reassignRepo(ctx context.Context, tx *db.Tx, rowid int64) (ok bool, err error) {
	var admin int64
	err = tx.GetContext(ctx, &admin, `SELECT id FROM users WHERE admin = ? ORDER BY id LIMIT 1`, true)
	if errors.Is(err, db.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up an admin for an ownerless repository: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repos SET user_id = ? WHERE rowid = ?`, admin, rowid); err != nil {
		return false, fmt.Errorf("give an ownerless repository to the first admin: %w", err)
	}
	return true, nil
}
