package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	mirrorSyncStatusName    = "mirror_sync_status"
	mirrorSyncStatusVersion = 9
)

// Record the outcome of each mirror's syncs: when it last synced, and the
// error of the last sync that failed, empty once one succeeds. A mirror whose
// remote went away was retried at every tick with nothing but a log line to
// show for it.
var mirrorSyncStatus = Migration{
	Name:    mirrorSyncStatusName,
	Version: mirrorSyncStatusVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, mirrorSyncStatusVersion, mirrorSyncStatusName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, mirrorSyncStatusVersion, mirrorSyncStatusName)
	},
}
