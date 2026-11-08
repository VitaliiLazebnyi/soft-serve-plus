package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	migrateLfsObjectsName    = "migrate_lfs_objects"
	migrateLfsObjectsVersion = 3
)

// Correct LFS objects relative path.
// From OID[:2]/OID[2:4]/OID[4:] to OID[:2]/OID[2:4]/OID
// See: https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md#intercepting-git
var migrateLfsObjects = Migration{
	Name:    migrateLfsObjectsName,
	Version: migrateLfsObjectsVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		cfg := config.FromContext(ctx)
		logger := log.FromContext(ctx).WithPrefix("migrate_lfs_objects")

		var repoIDs []int64
		if err := tx.SelectContext(ctx, &repoIDs, "SELECT id FROM repos"); err != nil {
			return err
		}
		for _, r := range repoIDs {
			var oids []string
			if err := tx.SelectContext(ctx, &oids, tx.Rebind("SELECT oid FROM lfs_objects WHERE repo_id = ?"), r); err != nil {
				return err
			}
			objsp := filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(r, 10), "objects")
			for _, oid := range oids {
				oldpath := filepath.Join(objsp, badRelativePath(oid))
				newpath := filepath.Join(objsp, goodRelativePath(oid))
				if _, err := os.Stat(oldpath); err == nil {
					if err := os.Rename(oldpath, newpath); err != nil {
						logger.Error("rename lfs object", "oldpath", oldpath, "newpath", newpath, "err", err)
						continue
					}
				}
			}
		}
		return nil
	},
	// Nothing to undo: moving the objects back to the wrong paths would
	// break LFS, and running the migration again is harmless.
	Rollback: func(context.Context, *db.Tx) error {
		return nil
	},
}

func goodRelativePath(oid string) string {
	if len(oid) < 5 {
		return oid
	}
	return filepath.Join(oid[:2], oid[2:4], oid)
}

func badRelativePath(oid string) string {
	if len(oid) < 5 {
		return oid
	}
	return filepath.Join(oid[:2], oid[2:4], oid[4:])
}
