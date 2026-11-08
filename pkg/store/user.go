package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"golang.org/x/crypto/ssh"
)

// UserStore is an interface for managing users.
type UserStore interface {
	GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error)
	FindUserByUsername(ctx context.Context, h db.Handler, username string) (models.User, error)
	// FindUserByUsernameForUpdate is FindUserByUsername that also locks the
	// row until the transaction ends, on Postgres, with FOR UPDATE: the only
	// row lock that also holds off the key-share lock a foreign key check
	// takes, so no repository, public key or token can be added for the user
	// meanwhile, and one inserted before the lock was taken is committed (or
	// rolled back) by the time it is granted. On SQLite a writing
	// transaction already excludes other writers, and it is
	// FindUserByUsername.
	FindUserByUsernameForUpdate(ctx context.Context, h db.Handler, username string) (models.User, error)
	FindUserByPublicKey(ctx context.Context, h db.Handler, pk ssh.PublicKey) (models.User, error)
	FindUserByAccessToken(ctx context.Context, h db.Handler, token string) (models.User, error)
	GetAllUsers(ctx context.Context, h db.Handler) ([]models.User, error)
	// FindDefaultAdminUserID returns the ID of the lowest-ID admin user. It
	// returns sql.ErrNoRows if no user is an admin.
	FindDefaultAdminUserID(ctx context.Context, h db.Handler) (int64, error)
	CreateUser(ctx context.Context, h db.Handler, username string, isAdmin bool, pks []ssh.PublicKey) error
	DeleteUserByUsername(ctx context.Context, h db.Handler, username string) error
	SetUsernameByUsername(ctx context.Context, h db.Handler, username string, newUsername string) error
	SetAdminByUsername(ctx context.Context, h db.Handler, username string, isAdmin bool) error
	AddPublicKeyByUsername(ctx context.Context, h db.Handler, username string, pk ssh.PublicKey) error
	RemovePublicKeyByUsername(ctx context.Context, h db.Handler, username string, pk ssh.PublicKey) error
	ListPublicKeysByUserID(ctx context.Context, h db.Handler, id int64) ([]ssh.PublicKey, error)
	ListPublicKeysByUsername(ctx context.Context, h db.Handler, username string) ([]ssh.PublicKey, error)
	SetUserPassword(ctx context.Context, h db.Handler, userID int64, password string) error
	SetUserPasswordByUsername(ctx context.Context, h db.Handler, username string, password string) error
}
