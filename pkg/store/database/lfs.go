package database

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// CreateLFSLockForUser implements store.Store.
func (*Store) CreateLFSLockForUser(ctx context.Context, tx db.Handler, repoID int64, userID int64, path string, refname string) error {
	query := tx.Rebind(`INSERT INTO lfs_locks (repo_id, user_id, path, refname, updated_at)
		VALUES (
			?,
			?,
			?,
			?,
			CURRENT_TIMESTAMP
		);
	`)
	_, err := tx.ExecContext(ctx, query, repoID, userID, path, refname)
	return err
}

// GetLFSLocks implements store.Store.
//
// It returns up to limit locks with an ID above afterID, in ID order. Paging
// by ID gives every lock one place in a stable order, unlike a row offset
// over the one-second updated_at, so a lock is never skipped or listed twice,
// even when locks are created or deleted between pages.
func (*Store) GetLFSLocks(ctx context.Context, tx db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error) {
	// SQLite treats a negative LIMIT as no limit and Postgres rejects it, so
	// a non-positive limit returns no locks on both, as LIMIT 0 would.
	if limit <= 0 {
		return []models.LFSLock{}, nil
	}

	var locks []models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?;
	`)
	err := tx.SelectContext(ctx, &locks, query, repoID, afterID, limit)
	return locks, err
}

// GetLFSLockForPath implements store.Store.
func (*Store) GetLFSLockForPath(ctx context.Context, tx db.Handler, repoID int64, path string) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND path = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, repoID, path)
	return lock, err
}

// GetLFSLockForUserPath implements store.Store.
func (*Store) GetLFSLockForUserPath(ctx context.Context, tx db.Handler, repoID int64, userID int64, path string) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND user_id = ? AND path = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, repoID, userID, path)
	return lock, err
}

// GetLFSLockByID implements store.Store.
func (*Store) GetLFSLockByID(ctx context.Context, tx db.Handler, repoID int64, id int64) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE id = ? AND repo_id = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, id, repoID)
	return lock, err
}

// GetLFSLockForUserByID implements store.Store.
func (*Store) GetLFSLockForUserByID(ctx context.Context, tx db.Handler, repoID int64, userID int64, id int64) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE id = ? AND user_id = ? AND repo_id = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, id, userID, repoID)
	return lock, err
}

// DeleteLFSLockForUserByID implements store.Store.
func (*Store) DeleteLFSLockForUserByID(ctx context.Context, tx db.Handler, repoID int64, userID int64, id int64) error {
	query := tx.Rebind(`
		DELETE FROM lfs_locks
		WHERE repo_id = ? AND user_id = ? AND id = ?;
	`)
	_, err := tx.ExecContext(ctx, query, repoID, userID, id)
	return err
}

// DeleteLFSLock implements store.Store.
func (*Store) DeleteLFSLock(ctx context.Context, tx db.Handler, repoID int64, id int64) error {
	query := tx.Rebind(`
		DELETE FROM lfs_locks
		WHERE repo_id = ? AND id = ?;
	`)
	_, err := tx.ExecContext(ctx, query, repoID, id)
	return err
}

// CreateLFSObject implements store.Store.
func (*Store) CreateLFSObject(ctx context.Context, tx db.Handler, repoID int64, oid string, size int64) error {
	query := tx.Rebind(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP);`)
	_, err := tx.ExecContext(ctx, query, repoID, oid, size)
	return err
}

// UpdateLFSObjectSize implements store.Store.
func (*Store) UpdateLFSObjectSize(ctx context.Context, tx db.Handler, repoID int64, oid string, size int64) error {
	query := tx.Rebind(`UPDATE lfs_objects SET size = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_id = ? AND oid = ?;`)
	_, err := tx.ExecContext(ctx, query, size, repoID, oid)
	return err
}

// GetLFSObjectByOid implements store.Store.
func (*Store) GetLFSObjectByOid(ctx context.Context, tx db.Handler, repoID int64, oid string) (models.LFSObject, error) {
	var obj models.LFSObject
	query := tx.Rebind(`SELECT * FROM lfs_objects WHERE repo_id = ? AND oid = ?;`)
	err := tx.GetContext(ctx, &obj, query, repoID, oid)
	return obj, err
}

// GetLFSObjects implements store.Store.
func (*Store) GetLFSObjects(ctx context.Context, tx db.Handler, repoID int64) ([]models.LFSObject, error) {
	var objs []models.LFSObject
	query := tx.Rebind(`SELECT * FROM lfs_objects WHERE repo_id = ? ORDER BY id;`)
	err := tx.SelectContext(ctx, &objs, query, repoID)
	return objs, err
}
