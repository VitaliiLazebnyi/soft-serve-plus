package database

import (
	"context"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

type lfsStore struct{}

var _ store.LFSStore = (*lfsStore)(nil)

func sanitizePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	return path
}

// CreateLFSLockForUser implements store.LFSStore.
func (*lfsStore) CreateLFSLockForUser(ctx context.Context, tx db.Handler, repoID int64, userID int64, path string, refname string) error {
	path = sanitizePath(path)
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

// GetLFSLocks implements store.LFSStore.
//
// It returns up to limit locks with an ID above afterID, in ID order. Paging
// by ID gives every lock one place in a stable order, unlike a row offset
// over the one-second updated_at, so a lock is never skipped or listed twice,
// even when locks are created or deleted between pages.
func (*lfsStore) GetLFSLocks(ctx context.Context, tx db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error) {
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

// GetLFSLocksWithCount implements store.LFSStore.
func (s *lfsStore) GetLFSLocksWithCount(ctx context.Context, tx db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, int64, error) {
	locks, err := s.GetLFSLocks(ctx, tx, repoID, afterID, limit)
	if err != nil {
		return nil, 0, err
	}

	var count int64
	query := tx.Rebind(`
		SELECT COUNT(*)
		FROM lfs_locks
		WHERE repo_id = ?;
	`)
	err = tx.GetContext(ctx, &count, query, repoID)
	if err != nil {
		return nil, 0, err
	}

	return locks, count, nil
}

// GetLFSLocksForUser implements store.LFSStore.
func (*lfsStore) GetLFSLocksForUser(ctx context.Context, tx db.Handler, repoID int64, userID int64) ([]models.LFSLock, error) {
	var locks []models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND user_id = ?;
	`)
	err := tx.SelectContext(ctx, &locks, query, repoID, userID)
	return locks, err
}

// GetLFSLocksForPath implements store.LFSStore.
func (*lfsStore) GetLFSLockForPath(ctx context.Context, tx db.Handler, repoID int64, path string) (models.LFSLock, error) {
	path = sanitizePath(path)
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND path = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, repoID, path)
	return lock, err
}

// GetLFSLockForUserPath implements store.LFSStore.
func (*lfsStore) GetLFSLockForUserPath(ctx context.Context, tx db.Handler, repoID int64, userID int64, path string) (models.LFSLock, error) {
	path = sanitizePath(path)
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE repo_id = ? AND user_id = ? AND path = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, repoID, userID, path)
	return lock, err
}

// GetLFSLockByID implements store.LFSStore.
func (*lfsStore) GetLFSLockByID(ctx context.Context, tx db.Handler, repoID int64, id int64) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE id = ? AND repo_id = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, id, repoID)
	return lock, err
}

// GetLFSLockForUserByID implements store.LFSStore.
func (*lfsStore) GetLFSLockForUserByID(ctx context.Context, tx db.Handler, repoID int64, userID int64, id int64) (models.LFSLock, error) {
	var lock models.LFSLock
	query := tx.Rebind(`
		SELECT *
		FROM lfs_locks
		WHERE id = ? AND user_id = ? AND repo_id = ?;
	`)
	err := tx.GetContext(ctx, &lock, query, id, userID, repoID)
	return lock, err
}

// DeleteLFSLockForUserByID implements store.LFSStore.
func (*lfsStore) DeleteLFSLockForUserByID(ctx context.Context, tx db.Handler, repoID int64, userID int64, id int64) error {
	query := tx.Rebind(`
		DELETE FROM lfs_locks
		WHERE repo_id = ? AND user_id = ? AND id = ?;
	`)
	_, err := tx.ExecContext(ctx, query, repoID, userID, id)
	return err
}

// DeleteLFSLock implements store.LFSStore.
func (*lfsStore) DeleteLFSLock(ctx context.Context, tx db.Handler, repoID int64, id int64) error {
	query := tx.Rebind(`
		DELETE FROM lfs_locks
		WHERE repo_id = ? AND id = ?;
	`)
	_, err := tx.ExecContext(ctx, query, repoID, id)
	return err
}

// CreateLFSObject implements store.LFSStore.
func (*lfsStore) CreateLFSObject(ctx context.Context, tx db.Handler, repoID int64, oid string, size int64) error {
	query := tx.Rebind(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP);`)
	_, err := tx.ExecContext(ctx, query, repoID, oid, size)
	return err
}

// DeleteLFSObjectByOid implements store.LFSStore.
func (*lfsStore) DeleteLFSObjectByOid(ctx context.Context, tx db.Handler, repoID int64, oid string) error {
	query := tx.Rebind(`DELETE FROM lfs_objects WHERE repo_id = ? AND oid = ?;`)
	_, err := tx.ExecContext(ctx, query, repoID, oid)
	return err
}

// UpdateLFSObjectSize implements store.LFSStore.
func (*lfsStore) UpdateLFSObjectSize(ctx context.Context, tx db.Handler, repoID int64, oid string, size int64) error {
	query := tx.Rebind(`UPDATE lfs_objects SET size = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_id = ? AND oid = ?;`)
	_, err := tx.ExecContext(ctx, query, size, repoID, oid)
	return err
}

// GetLFSObjectByOid implements store.LFSStore.
func (*lfsStore) GetLFSObjectByOid(ctx context.Context, tx db.Handler, repoID int64, oid string) (models.LFSObject, error) {
	var obj models.LFSObject
	query := tx.Rebind(`SELECT * FROM lfs_objects WHERE repo_id = ? AND oid = ?;`)
	err := tx.GetContext(ctx, &obj, query, repoID, oid)
	return obj, err
}

// GetLFSObjects implements store.LFSStore.
func (*lfsStore) GetLFSObjects(ctx context.Context, tx db.Handler, repoID int64) ([]models.LFSObject, error) {
	var objs []models.LFSObject
	query := tx.Rebind(`SELECT * FROM lfs_objects WHERE repo_id = ? ORDER BY id;`)
	err := tx.SelectContext(ctx, &objs, query, repoID)
	return objs, err
}

// GetLFSObjectsByName implements store.LFSStore.
func (*lfsStore) GetLFSObjectsByName(ctx context.Context, tx db.Handler, name string) ([]models.LFSObject, error) {
	var objs []models.LFSObject
	query := tx.Rebind(`
		SELECT lfs_objects.*
		FROM lfs_objects
		INNER JOIN repos ON lfs_objects.repo_id = repos.id
		WHERE repos.name = ?
		ORDER BY lfs_objects.id;
	`)
	err := tx.SelectContext(ctx, &objs, query, name)
	return objs, err
}
