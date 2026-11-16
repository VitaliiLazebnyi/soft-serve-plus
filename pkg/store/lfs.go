package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// LFSStore is the interface for the LFS store.
type LFSStore interface {
	CreateLFSObject(ctx context.Context, h db.Handler, repoID int64, oid string, size int64) error
	GetLFSObjectByOid(ctx context.Context, h db.Handler, repoID int64, oid string) (models.LFSObject, error)
	// GetLFSObjectsByOids returns the repository's objects among oids, in
	// one round trip for up to 500 of them; oids it does not have are left
	// out.
	GetLFSObjectsByOids(ctx context.Context, h db.Handler, repoID int64, oids []string) ([]models.LFSObject, error)
	GetLFSObjects(ctx context.Context, h db.Handler, repoID int64) ([]models.LFSObject, error)
	UpdateLFSObjectSize(ctx context.Context, h db.Handler, repoID int64, oid string, size int64) error

	// CreateLFSLockForUser locks path, stored and looked up as given: the
	// caller validates it with lfs.ValidateLockPath. It returns the lock, with
	// OwnerName left empty: the caller has the user.
	CreateLFSLockForUser(ctx context.Context, h db.Handler, repoID int64, userID int64, path string, refname string) (models.LFSLock, error)

	// The lock reads below fill in each lock's OwnerName.

	// GetLFSLocks returns up to limit locks of the repository with an ID
	// above afterID, in ID order; afterID 0 starts at the first lock.
	GetLFSLocks(ctx context.Context, h db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error)
	GetLFSLockForPath(ctx context.Context, h db.Handler, repoID int64, path string) (models.LFSLock, error)
	GetLFSLockForUserPath(ctx context.Context, h db.Handler, repoID int64, userID int64, path string) (models.LFSLock, error)
	GetLFSLockByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.LFSLock, error)
	GetLFSLockForUserByID(ctx context.Context, h db.Handler, repoID int64, userID int64, id int64) (models.LFSLock, error)
	DeleteLFSLock(ctx context.Context, h db.Handler, repoID int64, id int64) error
	DeleteLFSLockForUserByID(ctx context.Context, h db.Handler, repoID int64, userID int64, id int64) error
}
