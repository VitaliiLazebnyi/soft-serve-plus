package database

import (
	"context"
	"slices"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// CreateLFSLockForUser implements store.Store.
func (*Store) CreateLFSLockForUser(ctx context.Context, h db.Handler, repoID int64, userID int64, path string, refname string) (models.LFSLock, error) {
	var lock models.LFSLock
	query := h.Rebind(`INSERT INTO lfs_locks (repo_id, user_id, path, refname, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		RETURNING ` + lfsLockColumns + `;`)
	err := h.GetContext(ctx, &lock, query, repoID, userID, path, refname)
	return lock, err
}

// lfsLocks selects locks with their owner's username, for the lock reads.
//
// The queries compare lfs_locks.id with CAST(? AS BIGINT), not with ?. A
// lock ID or cursor comes from the client, and lfs_locks.id is a 32-bit
// SERIAL on Postgres, which types a bare parameter after the column and
// refuses a value past 2^31-1 as out of range. As BIGINT it compares like
// any other number, so such an ID names no lock, as it does on SQLite.
const lfsLocks = `SELECT ` + lfsLockColumnsQualified + `, users.username AS owner_name
	FROM lfs_locks INNER JOIN users ON users.id = lfs_locks.user_id`

// GetLFSLocks implements store.Store.
//
// It returns up to limit locks with an ID above afterID, in ID order. Paging
// by ID gives every lock one place in a stable order, unlike a row offset
// over the one-second updated_at, so a lock is never skipped or listed twice,
// even when locks are created or deleted between pages.
func (*Store) GetLFSLocks(ctx context.Context, h db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error) {
	// SQLite treats a negative LIMIT as no limit and Postgres rejects it, so
	// a non-positive limit returns no locks on both, as LIMIT 0 would.
	if limit <= 0 {
		return []models.LFSLock{}, nil
	}

	var locks []models.LFSLock
	query := h.Rebind(lfsLocks + `
		WHERE lfs_locks.repo_id = ? AND lfs_locks.id > CAST(? AS BIGINT)
		ORDER BY lfs_locks.id ASC
		LIMIT ?;
	`)
	err := h.SelectContext(ctx, &locks, query, repoID, afterID, limit)
	return locks, err
}

// GetLFSLockForPath implements store.Store.
func (*Store) GetLFSLockForPath(ctx context.Context, h db.Handler, repoID int64, path string) (models.LFSLock, error) {
	var lock models.LFSLock
	query := h.Rebind(lfsLocks + `
		WHERE lfs_locks.repo_id = ? AND lfs_locks.path = ?;
	`)
	err := h.GetContext(ctx, &lock, query, repoID, path)
	return lock, err
}

// GetLFSLockByID implements store.Store.
func (*Store) GetLFSLockByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.LFSLock, error) {
	var lock models.LFSLock
	query := h.Rebind(lfsLocks + `
		WHERE lfs_locks.id = CAST(? AS BIGINT) AND lfs_locks.repo_id = ?;
	`)
	err := h.GetContext(ctx, &lock, query, id, repoID)
	return lock, err
}

// DeleteLFSLock implements store.Store.
func (*Store) DeleteLFSLock(ctx context.Context, h db.Handler, repoID int64, id int64) error {
	query := h.Rebind(`
		DELETE FROM lfs_locks
		WHERE repo_id = ? AND id = CAST(? AS BIGINT);
	`)
	_, err := h.ExecContext(ctx, query, repoID, id)
	return err
}

// CreateLFSObject implements store.Store.
func (*Store) CreateLFSObject(ctx context.Context, h db.Handler, repoID int64, oid string, size int64) error {
	query := h.Rebind(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP);`)
	_, err := h.ExecContext(ctx, query, repoID, oid, size)
	return err
}

// UpdateLFSObjectSize implements store.Store.
func (*Store) UpdateLFSObjectSize(ctx context.Context, h db.Handler, repoID int64, oid string, size int64) error {
	query := h.Rebind(`UPDATE lfs_objects SET size = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_id = ? AND oid = ?;`)
	_, err := h.ExecContext(ctx, query, size, repoID, oid)
	return err
}

// GetLFSObjectByOid implements store.Store.
func (*Store) GetLFSObjectByOid(ctx context.Context, h db.Handler, repoID int64, oid string) (models.LFSObject, error) {
	var obj models.LFSObject
	query := h.Rebind(`SELECT ` + lfsObjectColumns + ` FROM lfs_objects WHERE repo_id = ? AND oid = ?;`)
	err := h.GetContext(ctx, &obj, query, repoID, oid)
	return obj, err
}

// lfsObjectsByOidsChunk bounds the oids of one GetLFSObjectsByOids query,
// well under the bound parameters SQLite (32766) and Postgres (65535) take.
const lfsObjectsByOidsChunk = 500

// GetLFSObjectsByOids implements store.Store.
func (*Store) GetLFSObjectsByOids(ctx context.Context, h db.Handler, repoID int64, oids []string) ([]models.LFSObject, error) {
	objs := []models.LFSObject{}
	for chunk := range slices.Chunk(oids, lfsObjectsByOidsChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, oid := range chunk {
			args = append(args, oid)
		}
		var page []models.LFSObject
		query := h.Rebind(`SELECT ` + lfsObjectColumns + ` FROM lfs_objects WHERE repo_id = ? AND oid IN (` + placeholders(len(chunk)) + `) ORDER BY id;`)
		if err := h.SelectContext(ctx, &page, query, args...); err != nil {
			return nil, err
		}
		objs = append(objs, page...)
	}
	return objs, nil
}

// GetLFSObjects implements store.Store.
func (*Store) GetLFSObjects(ctx context.Context, h db.Handler, repoID int64) ([]models.LFSObject, error) {
	var objs []models.LFSObject
	query := h.Rebind(`SELECT ` + lfsObjectColumns + ` FROM lfs_objects WHERE repo_id = ? ORDER BY id;`)
	err := h.SelectContext(ctx, &objs, query, repoID)
	return objs, err
}
