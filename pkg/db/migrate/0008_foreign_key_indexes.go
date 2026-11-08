package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	foreignKeyIndexesName    = "foreign_key_indexes"
	foreignKeyIndexesVersion = 8
)

// Index foreign-key columns that no unique constraint leads with:
// public_keys.user_id (read on every user lookup, SSH authentication
// included), repos.user_id, collabs.repo_id, lfs_objects.repo_id,
// lfs_locks.user_id and access_tokens.user_id. Neither SQLite nor Postgres
// indexes a foreign-key column by itself, so those lookups and the cascades
// that run when a user or repository is deleted scanned the whole table.
var foreignKeyIndexes = Migration{
	Name:    foreignKeyIndexesName,
	Version: foreignKeyIndexesVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, foreignKeyIndexesVersion, foreignKeyIndexesName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, foreignKeyIndexesVersion, foreignKeyIndexesName)
	},
}
