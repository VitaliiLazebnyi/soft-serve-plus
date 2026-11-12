package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"golang.org/x/crypto/ssh"
)

type userStore struct{}

var _ store.UserStore = (*userStore)(nil)

// AddPublicKeyByUsername implements store.UserStore.
func (*userStore) AddPublicKeyByUsername(ctx context.Context, tx db.Handler, username string, pk ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	var userID int64
	if err := tx.GetContext(ctx, &userID, tx.Rebind(`SELECT id FROM users WHERE username = ?`), username); err != nil {
		return err
	}

	query := tx.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP);`)
	ak := sshutils.MarshalAuthorizedKey(pk)
	if _, err := tx.ExecContext(ctx, query, userID, ak); err != nil {
		return err
	}

	return forgetSyncedAdminKey(ctx, tx, ak)
}

// forgetSyncedAdminKey deletes the record that the initial_admin_keys sync
// attached ak, so a key added or removed by hand is no longer removed when it
// leaves the config.
func forgetSyncedAdminKey(ctx context.Context, tx db.Handler, ak string) error {
	_, err := tx.ExecContext(ctx, tx.Rebind(`DELETE FROM settings WHERE "key" = ?;`), db.SyncedAdminKeyPrefix+ak)
	return err
}

// CreateUser implements store.UserStore.
func (*userStore) CreateUser(ctx context.Context, tx db.Handler, username string, isAdmin bool, pks []ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := utils.ValidateNewUsername(username); err != nil {
		return err
	}

	query := tx.Rebind(`INSERT INTO users (username, admin, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP) RETURNING id;`)

	var userID int64
	if err := tx.GetContext(ctx, &userID, query, username, isAdmin); err != nil {
		return err
	}

	for _, pk := range pks {
		query := tx.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP);`)
		ak := sshutils.MarshalAuthorizedKey(pk)
		if _, err := tx.ExecContext(ctx, query, userID, ak); err != nil {
			return err
		}
		if err := forgetSyncedAdminKey(ctx, tx, ak); err != nil {
			return err
		}
	}

	return nil
}

// DeleteUserByUsername implements store.UserStore.
func (*userStore) DeleteUserByUsername(ctx context.Context, tx db.Handler, username string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := tx.Rebind(`DELETE FROM users WHERE username = ?;`)
	return affected(tx.ExecContext(ctx, query, username))
}

// GetUserByID implements store.UserStore.
func (*userStore) GetUserByID(ctx context.Context, tx db.Handler, id int64) (models.User, error) {
	var m models.User
	query := tx.Rebind(`SELECT * FROM users WHERE id = ?;`)
	err := tx.GetContext(ctx, &m, query, id)
	return m, err
}

// FindUserByPublicKey implements store.UserStore.
func (*userStore) FindUserByPublicKey(ctx context.Context, tx db.Handler, pk ssh.PublicKey) (models.User, error) {
	var m models.User
	query := tx.Rebind(`SELECT users.*
			FROM users
			INNER JOIN public_keys ON users.id = public_keys.user_id
			WHERE public_keys.public_key = ?;`)
	err := tx.GetContext(ctx, &m, query, sshutils.MarshalAuthorizedKey(pk))
	return m, err
}

// FindUserByUsername implements store.UserStore.
func (*userStore) FindUserByUsername(ctx context.Context, tx db.Handler, username string) (models.User, error) {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return models.User{}, err
	}

	var m models.User
	query := tx.Rebind(`SELECT * FROM users WHERE username = ?;`)
	err := tx.GetContext(ctx, &m, query, username)
	return m, err
}

// FindUserByUsernameForUpdate implements store.UserStore. See
// GetRepoByNameForUpdate.
func (*userStore) FindUserByUsernameForUpdate(ctx context.Context, h db.Handler, username string) (models.User, error) {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return models.User{}, err
	}

	var query string
	switch driver := h.DriverName(); driver {
	case "postgres":
		query = `SELECT * FROM users WHERE username = $1 FOR UPDATE;`
	case "sqlite":
		query = `SELECT * FROM users WHERE username = ?;`
	default:
		return models.User{}, fmt.Errorf("find user for update: unsupported database driver %q", driver)
	}
	var m models.User
	err := h.GetContext(ctx, &m, query, username)
	return m, err
}

// GetAllUsers implements store.UserStore.
func (*userStore) GetAllUsers(ctx context.Context, tx db.Handler) ([]models.User, error) {
	var ms []models.User
	query := tx.Rebind(`SELECT * FROM users ORDER BY username;`)
	err := tx.SelectContext(ctx, &ms, query)
	return ms, err
}

// FindDefaultAdminUserID implements store.UserStore.
//
// It reads a single row instead of every user, so registering an anonymous
// repository does not scan the users table inside its write transaction.
func (*userStore) FindDefaultAdminUserID(ctx context.Context, tx db.Handler) (int64, error) {
	var id int64
	query := tx.Rebind(`SELECT id FROM users WHERE admin = ? ORDER BY id ASC LIMIT 1;`)
	err := tx.GetContext(ctx, &id, query, true)
	return id, err
}

// ListPublicKeysByUserID implements store.UserStore.
func (*userStore) ListPublicKeysByUserID(ctx context.Context, tx db.Handler, id int64) ([]ssh.PublicKey, error) {
	var aks []string
	query := tx.Rebind(`SELECT public_key FROM public_keys
			WHERE user_id = ?
			ORDER BY public_keys.id ASC;`)
	err := tx.SelectContext(ctx, &aks, query, id)
	if err != nil {
		return nil, err
	}

	return parseStoredKeys(ctx, aks, "user_id", id), nil
}

// ListPublicKeysByUsername implements store.UserStore.
func (*userStore) ListPublicKeysByUsername(ctx context.Context, tx db.Handler, username string) ([]ssh.PublicKey, error) {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return nil, err
	}

	var aks []string
	query := tx.Rebind(`SELECT public_key FROM public_keys
			INNER JOIN users ON users.id = public_keys.user_id
			WHERE users.username = ?
			ORDER BY public_keys.id ASC;`)
	err := tx.SelectContext(ctx, &aks, query, username)
	if err != nil {
		return nil, err
	}

	return parseStoredKeys(ctx, aks, "username", username), nil
}

// RemovePublicKeyByUsername implements store.UserStore.
func (*userStore) RemovePublicKeyByUsername(ctx context.Context, tx db.Handler, username string, pk ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := tx.Rebind(`DELETE FROM public_keys
			WHERE user_id = (SELECT id FROM users WHERE username = ?)
			AND public_key = ?;`)
	ak := sshutils.MarshalAuthorizedKey(pk)
	// Nothing removed means the user does not have the key, and the sync
	// record (if any) still describes the key where it is.
	if err := affected(tx.ExecContext(ctx, query, username, ak)); err != nil {
		return err
	}
	return forgetSyncedAdminKey(ctx, tx, ak)
}

// SetAdminByUsername implements store.UserStore.
func (*userStore) SetAdminByUsername(ctx context.Context, tx db.Handler, username string, isAdmin bool) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := tx.Rebind(`UPDATE users SET admin = ?, updated_at = CURRENT_TIMESTAMP WHERE username = ?;`)
	return affected(tx.ExecContext(ctx, query, isAdmin, username))
}

// SetUsernameByUsername implements store.UserStore.
func (*userStore) SetUsernameByUsername(ctx context.Context, tx db.Handler, username string, newUsername string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	newUsername = strings.ToLower(newUsername)
	if err := utils.ValidateNewUsername(newUsername); err != nil {
		return err
	}

	query := tx.Rebind(`UPDATE users SET username = ?, updated_at = CURRENT_TIMESTAMP WHERE username = ?;`)
	return affected(tx.ExecContext(ctx, query, newUsername, username))
}

// SetUserPasswordByUsername implements store.UserStore.
func (*userStore) SetUserPasswordByUsername(ctx context.Context, tx db.Handler, username string, password string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := tx.Rebind(`UPDATE users SET password = ?, updated_at = CURRENT_TIMESTAMP WHERE username = ?;`)
	return affected(tx.ExecContext(ctx, query, password, username))
}

// affected returns the error of an UPDATE or DELETE, or db.ErrRecordNotFound
// when it matched no row, so a change to a missing record is not reported as
// done.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrRecordNotFound
	}
	return nil
}

// parseStoredKeys parses the stored authorized keys of one user. A key that
// does not parse (stored by an older version, or edited in the database) is
// skipped and logged, so it cannot fail every lookup of that user's keys and
// lock them out with their other, valid keys. keyvals name the user in the
// log entry.
func parseStoredKeys(ctx context.Context, aks []string, keyvals ...any) []ssh.PublicKey {
	pks := make([]ssh.PublicKey, 0, len(aks))
	for _, ak := range aks {
		pk, _, err := sshutils.ParseAuthorizedKey(ak)
		if err != nil {
			log.FromContext(ctx).WithPrefix("store").Warn("skipping unparsable stored public key", append(keyvals, "err", err)...)
			continue
		}
		pks = append(pks, pk)
	}
	return pks
}
