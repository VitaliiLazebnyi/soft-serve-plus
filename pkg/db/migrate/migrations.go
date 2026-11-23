package migrate

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

//go:embed *.sql
var sqls embed.FS

// Known differences between the SQLite and Postgres schemas, left on purpose
// because closing them would rewrite whole tables on upgrade for no change in
// behaviour:
//
//   - webhook_deliveries.id is TEXT UNIQUE on SQLite (since migration 4) but
//     TEXT PRIMARY KEY on Postgres, so SQLite would accept a NULL id. The only
//     writer, CreateWebhookDelivery, always sets a UUID. Adding NOT NULL on
//     SQLite needs another rebuild of the deliveries table, bodies included,
//     like the one migration 4 does.
//   - Ids from migrations 1 and 2 are SERIAL (32-bit) on Postgres but 64-bit
//     on SQLite. They number users, repos, keys, collaborators, webhooks and
//     LFS objects and locks, which stay far below 2^31. The one high-churn
//     counter, webhook_deliveries.seq, is BIGINT on both. Widening the ids
//     means altering every id and foreign key column, which rewrites each
//     table under an exclusive lock.
//
// Keep this in order of execution, oldest to newest.
var migrations = []Migration{
	createTables,
	webhooks,
	migrateLfsObjects,
	webhookDeliverySeq,
	webhookDeliveryRetention,
	repoAnonymous,
	lfsObjectSizeBigint,
	foreignKeyIndexes,
	mirrorSyncStatus,
	publicKeySyncedUser,
}

// The migrations that only run their SQL files.
var (
	// Add webhooks, their events and their deliveries.
	webhooks = sqlMigration(2, "webhooks")

	// Trim each webhook to its 100 most recent deliveries, the number
	// recording a delivery keeps from now on (maxDeliveries in pkg/webhook).
	// Deliveries had no retention before, so a table can hold far more.
	// Trimming it here, once, keeps that large delete out of recording a
	// delivery, which runs under a short timeout. Rolling back cannot
	// restore the deleted rows, so it does nothing.
	webhookDeliveryRetention = sqlMigration(5, "webhook_delivery_retention")

	// Mark repositories created without an account. They are owned by the
	// default admin (repos.user_id is NOT NULL), so the owner alone cannot
	// tell them apart from the admin's own; max_repos_per_user counts them
	// together as one user. Repositories that existed before this migration
	// are not marked, since nothing records how they were created.
	repoAnonymous = sqlMigration(6, "repo_anonymous")

	// Widen lfs_objects.size to 64 bits. Postgres created it as INTEGER,
	// which holds at most 2^31-1, so recording an LFS object of 2 GiB or
	// more failed. SQLite's INTEGER is already 64-bit, so its migration does
	// nothing.
	lfsObjectSizeBigint = sqlMigration(7, "lfs_object_size_bigint")

	// Index foreign-key columns that no unique constraint leads with:
	// public_keys.user_id (read on every user lookup, SSH authentication
	// included), repos.user_id, collabs.repo_id, lfs_objects.repo_id,
	// lfs_locks.user_id and access_tokens.user_id. Neither SQLite nor
	// Postgres indexes a foreign-key column by itself, so those lookups and
	// the cascades that run when a user or repository is deleted scanned the
	// whole table.
	foreignKeyIndexes = sqlMigration(8, "foreign_key_indexes")

	// Record the outcome of each mirror's syncs: when it last synced, and
	// the error of the last sync that failed, empty once one succeeds. A
	// mirror whose remote went away was retried at every tick with nothing
	// but a log line to show for it.
	mirrorSyncStatus = sqlMigration(9, "mirror_sync_status")

	// Record on the key itself that the initial_admin_keys sync attached it,
	// and to which user: public_keys.synced_user_id, NULL for every other
	// key. It replaces the settings rows "initial_admin_key:<key>" ->
	// "<user id>", which are carried over and deleted. A key deleted by hand
	// or with its user takes its record with it, which the settings rows
	// needed three store methods and a stale-record check for. Rolling back
	// writes the settings rows again. The column is not a foreign key: a
	// change of users.id cascades to user_id but not here, so the key then
	// looks moved to another account and is kept, which is the safe side.
	publicKeySyncedUser = sqlMigration(10, "public_key_synced_user")
)

// sqlMigration returns a migration that only runs its SQL files, an up and
// a down file for each driver, named as sqlFile says.
func sqlMigration(version int64, name string) Migration {
	return Migration{
		Version: version,
		Name:    name,
		Migrate: func(ctx context.Context, tx *db.Tx, _ Env) error {
			return execMigration(ctx, tx, version, name, false)
		},
		Rollback: func(ctx context.Context, tx *db.Tx, _ Env) error {
			return execMigration(ctx, tx, version, name, true)
		},
	}
}

// sqlFilePrefix returns what the names of a migration's SQL files start
// with, such as 0002_webhooks. Spaces in the name, as in migration 1's,
// become underscores.
func sqlFilePrefix(version int64, name string) string {
	return fmt.Sprintf("%04d_%s", version, strings.ReplaceAll(name, " ", "_"))
}

// sqlFile returns the name of a migration's SQL file for driver, in
// direction "up" or "down", such as 0002_webhooks_sqlite.up.sql.
func sqlFile(version int64, name, driver, direction string) string {
	return fmt.Sprintf("%s_%s.%s.sql", sqlFilePrefix(version, name), driver, direction)
}

// execMigration runs a migration's up or down SQL file for the
// transaction's driver.
func execMigration(ctx context.Context, tx *db.Tx, version int64, name string, down bool) error {
	direction := "up"
	if down {
		direction = "down"
	}
	sqlstr, err := sqls.ReadFile(sqlFile(version, name, tx.DriverName(), direction))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, string(sqlstr))
	return err
}
