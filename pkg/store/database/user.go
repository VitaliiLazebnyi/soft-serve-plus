package database

import (
	"context"
	"database/sql"
	"strings"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"golang.org/x/crypto/ssh"
)

// AddPublicKeyByUserID implements store.Store.
func (*Store) AddPublicKeyByUserID(ctx context.Context, h db.Handler, id int64, pk ssh.PublicKey) error {
	query := h.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP);`)
	_, err := h.ExecContext(ctx, query, id, sshutils.MarshalAuthorizedKey(pk))
	return err
}

// CreateUser implements store.Store.
func (*Store) CreateUser(ctx context.Context, h db.Handler, username string, isAdmin bool, pks []ssh.PublicKey) error {
	username = strings.ToLower(username)
	if err := utils.ValidateNewUsername(username); err != nil {
		return err
	}

	query := h.Rebind(`INSERT INTO users (username, admin, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP) RETURNING id;`)

	var userID int64
	if err := h.GetContext(ctx, &userID, query, username, isAdmin); err != nil {
		return err
	}

	for _, pk := range pks {
		query := h.Rebind(`INSERT INTO public_keys (user_id, public_key, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP);`)
		if _, err := h.ExecContext(ctx, query, userID, sshutils.MarshalAuthorizedKey(pk)); err != nil {
			return err
		}
	}

	return nil
}

// DeleteUserByUsername implements store.Store.
func (*Store) DeleteUserByUsername(ctx context.Context, h db.Handler, username string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := h.Rebind(`DELETE FROM users WHERE username = ?;`)
	return affected(h.ExecContext(ctx, query, username))
}

// GetUserByID implements store.Store.
func (*Store) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	var m models.User
	query := h.Rebind(`SELECT * FROM users WHERE id = ?;`)
	err := h.GetContext(ctx, &m, query, id)
	return m, err
}

// FindUserByPublicKey implements store.Store.
func (*Store) FindUserByPublicKey(ctx context.Context, h db.Handler, pk ssh.PublicKey) (models.User, error) {
	var m models.User
	query := h.Rebind(`SELECT users.*
			FROM users
			INNER JOIN public_keys ON users.id = public_keys.user_id
			WHERE public_keys.public_key = ?;`)
	err := h.GetContext(ctx, &m, query, sshutils.MarshalAuthorizedKey(pk))
	return m, err
}

// FindUserByUsername implements store.Store.
func (*Store) FindUserByUsername(ctx context.Context, h db.Handler, username string) (models.User, error) {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return models.User{}, err
	}

	var m models.User
	query := h.Rebind(`SELECT * FROM users WHERE username = ?;`)
	err := h.GetContext(ctx, &m, query, username)
	return m, err
}

// FindUserByUsernameForUpdate implements store.Store. See
// GetRepoByNameForUpdate.
func (*Store) FindUserByUsernameForUpdate(ctx context.Context, h db.Handler, username string) (models.User, error) {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return models.User{}, err
	}

	query := `SELECT * FROM users WHERE username = ?;`
	if h.DriverName() == "postgres" {
		query = `SELECT * FROM users WHERE username = $1 FOR UPDATE;`
	}
	var m models.User
	err := h.GetContext(ctx, &m, query, username)
	return m, err
}

// GetUserByIDForUpdate implements store.Store. See
// FindUserByUsernameForUpdate.
func (*Store) GetUserByIDForUpdate(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	query := `SELECT * FROM users WHERE id = ?;`
	if h.DriverName() == "postgres" {
		query = `SELECT * FROM users WHERE id = $1 FOR UPDATE;`
	}
	var m models.User
	err := h.GetContext(ctx, &m, query, id)
	return m, err
}

// GetAllUsers implements store.Store.
func (*Store) GetAllUsers(ctx context.Context, h db.Handler) ([]models.User, error) {
	var ms []models.User
	query := h.Rebind(`SELECT * FROM users ORDER BY username;`)
	err := h.SelectContext(ctx, &ms, query)
	return ms, err
}

// FindDefaultAdminUserID implements store.Store.
//
// It reads a single row instead of every user, so registering an anonymous
// repository does not scan the users table inside its write transaction.
func (*Store) FindDefaultAdminUserID(ctx context.Context, h db.Handler) (int64, error) {
	var id int64
	query := h.Rebind(`SELECT id FROM users WHERE admin = ? ORDER BY id ASC LIMIT 1;`)
	err := h.GetContext(ctx, &id, query, true)
	return id, err
}

// ListPublicKeysByUserID implements store.Store.
func (*Store) ListPublicKeysByUserID(ctx context.Context, h db.Handler, id int64) ([]ssh.PublicKey, error) {
	var aks []string
	query := h.Rebind(`SELECT public_key FROM public_keys
			WHERE user_id = ?
			ORDER BY public_keys.id ASC;`)
	err := h.SelectContext(ctx, &aks, query, id)
	if err != nil {
		return nil, err
	}

	return parseStoredKeys(ctx, aks, "user_id", id), nil
}

// RemovePublicKeyByUserID implements store.Store.
func (*Store) RemovePublicKeyByUserID(ctx context.Context, h db.Handler, id int64, pk ssh.PublicKey) error {
	query := h.Rebind(`DELETE FROM public_keys WHERE user_id = ? AND public_key = ?;`)
	// Nothing removed means the user does not have the key.
	return affected(h.ExecContext(ctx, query, id, sshutils.MarshalAuthorizedKey(pk)))
}

// SetAdminByUsername implements store.Store.
func (*Store) SetAdminByUsername(ctx context.Context, h db.Handler, username string, isAdmin bool) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	query := h.Rebind(`UPDATE users SET admin = ?, updated_at = CURRENT_TIMESTAMP WHERE username = ?;`)
	return affected(h.ExecContext(ctx, query, isAdmin, username))
}

// SetUsernameByUsername implements store.Store.
func (*Store) SetUsernameByUsername(ctx context.Context, h db.Handler, username string, newUsername string) error {
	username = strings.ToLower(username)
	if err := utils.ValidateUsername(username); err != nil {
		return err
	}

	newUsername = strings.ToLower(newUsername)
	if err := utils.ValidateNewUsername(newUsername); err != nil {
		return err
	}

	query := h.Rebind(`UPDATE users SET username = ?, updated_at = CURRENT_TIMESTAMP WHERE username = ?;`)
	return affected(h.ExecContext(ctx, query, newUsername, username))
}

// SetUsernameByUserID implements store.Store.
func (*Store) SetUsernameByUserID(ctx context.Context, h db.Handler, id int64, newUsername string) error {
	newUsername = strings.ToLower(newUsername)
	if err := utils.ValidateNewUsername(newUsername); err != nil {
		return err
	}

	query := h.Rebind(`UPDATE users SET username = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?;`)
	return affected(h.ExecContext(ctx, query, newUsername, id))
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
