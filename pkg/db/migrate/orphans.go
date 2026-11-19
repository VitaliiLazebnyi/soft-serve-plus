package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/jmoiron/sqlx"
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

// ownerlessRepo is a repository whose owner no longer exists.
type ownerlessRepo struct {
	ID     int64  `db:"id"`
	Name   string `db:"name"`
	UserID int64  `db:"user_id"`
}

// loggedIDTables are the tables whose removed rows are logged by id: rows
// that grant access or send data somewhere.
var loggedIDTables = map[string]bool{"public_keys": true, "access_tokens": true, "webhooks": true}

// removeSqliteOrphans removes the rows of a SQLite database that refer to a
// row that does not exist, before any pending migration runs.
//
// Foreign keys used to be enforced only by the default data source, so a
// database used with a data source that left them off can hold such rows:
// the keys and tokens of a deleted user, the collaborators, webhooks, LFS
// objects and locks of a deleted repository, the deliveries of a deleted
// webhook. With foreign keys now always enforced, they made the first
// migration that copies a table fail, and an orphaned key could not be added
// to another user.
//
// A repository whose owner was deleted is still a repository, with a
// directory, so it is given to the first admin, and its name and former
// owner id are logged. With no admin at all, nothing is removed and an error
// names the repositories, so the operator can make a user an admin first.
// Every other such row is removed, with the rows ON DELETE CASCADE removes
// with it; each table's count is logged, and the ids of removed keys, access
// tokens and webhooks.
//
// It runs when migrations are pending, and otherwise until a check finds the
// database clean, which is marked in settings: the check reads every row
// that refers to another, and with foreign keys enforced no new orphans
// appear. A database rolled back and holding new orphans has migrations
// pending again, so it is checked again.
func removeSqliteOrphans(ctx context.Context, tx *db.Tx, logger *log.Logger, pending bool) error {
	if tx.DriverName() != "sqlite" {
		return nil
	}
	hasSettings, err := tableExists(ctx, tx, "settings")
	if err != nil {
		return fmt.Errorf("look up settings table: %w", err)
	}
	if hasSettings && !pending {
		var n int
		if err := tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM settings WHERE "key" = ?`, orphansCheckedKey); err != nil {
			return fmt.Errorf("look up the orphan check: %w", err)
		}
		if n > 0 {
			return nil
		}
	}

	if err := reassignOwnerlessRepos(ctx, tx, logger); err != nil {
		return err
	}

	before, err := tableCounts(ctx, tx)
	if err != nil {
		return err
	}
	removedIDs := map[string][]int64{}
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
			table := quoteIdent(r.Table)
			if loggedIDTables[r.Table] {
				var id int64
				err := tx.GetContext(ctx, &id, "SELECT id FROM "+table+" WHERE rowid = ?", r.RowID.Int64)
				if err == nil {
					removedIDs[r.Table] = append(removedIDs[r.Table], id)
				} else if !errors.Is(err, db.ErrRecordNotFound) {
					return fmt.Errorf("look up a %s row referring to a missing %s row: %w", r.Table, r.Parent, err)
				}
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rowid = ?", r.RowID.Int64); err != nil {
				return fmt.Errorf("remove a %s row referring to a missing %s row: %w", r.Table, r.Parent, err)
			}
		}
	}
	after, err := tableCounts(ctx, tx)
	if err != nil {
		return err
	}

	for _, t := range slices.Sorted(maps.Keys(before)) {
		if n := before[t] - after[t]; n > 0 {
			kv := []any{"table", t, "rows", n}
			if ids := removedIDs[t]; len(ids) > 0 {
				kv = append(kv, "ids", ids)
			}
			logger.Warn("removed rows that referred to rows that no longer exist, counting the rows removed with them", kv...)
		}
	}
	if hasSettings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings ("key", value, updated_at) VALUES (?, 'true', CURRENT_TIMESTAMP)
			ON CONFLICT ("key") DO NOTHING`, orphansCheckedKey); err != nil {
			return fmt.Errorf("record the orphan check: %w", err)
		}
	}
	return nil
}

// reassignOwnerlessRepos gives every repository whose owner no longer exists
// to the first admin, logging each one's name and former owner id. With no
// admin it changes nothing and returns an error that names them.
func reassignOwnerlessRepos(ctx context.Context, tx *db.Tx, logger *log.Logger) error {
	ok, err := tableExists(ctx, tx, "repos")
	if err != nil || !ok {
		return err
	}
	var repos []ownerlessRepo
	if err := tx.SelectContext(ctx, &repos, `SELECT id, name, user_id FROM repos
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE users.id = repos.user_id) ORDER BY name`); err != nil {
		return fmt.Errorf("list repositories whose owner no longer exists: %w", err)
	}
	if len(repos) == 0 {
		return nil
	}

	var admin int64
	err = tx.GetContext(ctx, &admin, `SELECT id FROM users WHERE admin = ? ORDER BY id LIMIT 1`, true)
	if errors.Is(err, db.ErrRecordNotFound) {
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, fmt.Sprintf("%s (owner id %d)", r.Name, r.UserID))
		}
		file, err := sqliteFile(ctx, tx)
		if err != nil {
			return err
		}
		return fmt.Errorf("the owners of these repositories no longer exist and there is no admin to give them to: %s; nothing was changed. "+
			"Make a user an admin, then start again. "+
			"With the previous version running, before upgrading, as an admin with a key listed in initial_admin_keys: "+
			"ssh -p PORT HOST user set-admin USERNAME true. "+
			"With the server stopped: sqlite3 %s \"UPDATE users SET admin = 1 WHERE username = 'USERNAME'\". "+
			"With no users at all, stop the server and add an admin: sqlite3 %[2]s \"INSERT INTO users (username, admin, updated_at) VALUES ('admin', 1, CURRENT_TIMESTAMP)\"",
			strings.Join(names, ", "), file)
	}
	if err != nil {
		return fmt.Errorf("look up an admin for repositories whose owner no longer exists: %w", err)
	}
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `UPDATE repos SET user_id = ? WHERE id = ?`, admin, r.ID); err != nil {
			return fmt.Errorf("give repository %s to the first admin: %w", r.Name, err)
		}
		logger.Warn("gave a repository whose owner no longer exists to the first admin",
			"repo", r.Name, "former_user_id", r.UserID, "user_id", admin)
	}
	return nil
}

// sqliteFile returns the path of the main database file, quoted for this
// system's shell when it needs to be, for a message that tells the operator
// what to run. An in-memory database, which has no path, gives a description
// instead.
func sqliteFile(ctx context.Context, tx *db.Tx) (string, error) {
	file, err := sqliteFilePath(ctx, tx)
	if err != nil {
		return "", err
	}
	if file == "" {
		return "DATABASE_FILE (the file db.data_source names)", nil
	}
	return shellQuote(runtime.GOOS, file), nil
}

// sqliteFilePath returns the path of the main database file as SQLite has
// it, or "" for an in-memory database.
func sqliteFilePath(ctx context.Context, tx sqlx.QueryerContext) (string, error) {
	var file string
	if err := sqlx.GetContext(ctx, tx, &file, `SELECT file FROM pragma_database_list WHERE name = 'main'`); err != nil {
		return "", fmt.Errorf("look up the database file: %w", err)
	}
	return file, nil
}

// shellQuote quotes path for a command line on goos, when it holds anything
// outside a set that needs no quoting. Elsewhere than on Windows that is a
// POSIX shell's single quotes, a single quote in path closing them, written
// escaped and opening them again. On Windows it is double quotes, which
// cmd.exe and PowerShell both take and which a Windows path cannot contain;
// cmd.exe still expands %NAME% in them, and PowerShell $name and backticks.
func shellQuote(goos, path string) string {
	safe := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@,%="
	if goos == "windows" {
		safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\\/._-:"
	}
	if path != "" && strings.IndexFunc(path, func(r rune) bool { return !strings.ContainsRune(safe, r) }) < 0 {
		return path
	}
	if goos == "windows" {
		return `"` + path + `"`
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// tableCounts returns the number of rows of every table of the database.
func tableCounts(ctx context.Context, tx *db.Tx) (map[string]int64, error) {
	var tables []string
	if err := tx.SelectContext(ctx, &tables, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	counts := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := tx.GetContext(ctx, &n, "SELECT COUNT(*) FROM "+quoteIdent(t)); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		counts[t] = n
	}
	return counts, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
