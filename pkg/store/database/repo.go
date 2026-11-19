package database

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// CreateRepo implements store.Store.
func (*Store) CreateRepo(ctx context.Context, tx db.Handler, r models.Repo) (models.Repo, error) {
	if r.UserID <= 0 {
		// repos.user_id is NOT NULL and references users.
		return models.Repo{}, fmt.Errorf("create repo %q: invalid owner user ID %d", r.Name, r.UserID)
	}
	var m models.Repo
	query := tx.Rebind(`INSERT INTO repos (name, project_name, description, private, mirror, hidden, anonymous, updated_at, user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
			RETURNING *;`)
	err := tx.GetContext(ctx, &m, query, utils.SanitizeRepo(r.Name), r.ProjectName, r.Description, r.Private, r.Mirror, r.Hidden, r.Anonymous, r.UserID)
	return m, err
}

// DeleteRepoByName implements store.Store.
func (*Store) DeleteRepoByName(ctx context.Context, tx db.Handler, name string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("DELETE FROM repos WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, name))
}

// GetAllRepos implements store.Store.
func (*Store) GetAllRepos(ctx context.Context, tx db.Handler) ([]models.Repo, error) {
	var repos []models.Repo
	query := tx.Rebind("SELECT * FROM repos ORDER BY name;")
	err := tx.SelectContext(ctx, &repos, query)
	return repos, err
}

// GetUserRepos implements store.Store.
func (*Store) GetUserRepos(ctx context.Context, tx db.Handler, userID int64) ([]models.Repo, error) {
	var repos []models.Repo
	query := tx.Rebind("SELECT * FROM repos WHERE user_id = ? ORDER BY name;")
	err := tx.SelectContext(ctx, &repos, query, userID)
	return repos, err
}

// CountUserRepos implements store.Store.
func (*Store) CountUserRepos(ctx context.Context, tx db.Handler, userID int64) (int64, error) {
	var n int64
	query := tx.Rebind("SELECT COUNT(*) FROM repos WHERE user_id = ? AND anonymous = ?;")
	err := tx.GetContext(ctx, &n, query, userID, false)
	return n, err
}

// CountAnonymousRepos implements store.Store.
func (*Store) CountAnonymousRepos(ctx context.Context, tx db.Handler) (int64, error) {
	var n int64
	query := tx.Rebind("SELECT COUNT(*) FROM repos WHERE anonymous = ?;")
	err := tx.GetContext(ctx, &n, query, true)
	return n, err
}

// LockUserRepos implements store.Store. See LockAndCountWebhooks
// for why FOR NO KEY UPDATE: it does not block the key-share lock that
// inserting a repository owned by the user takes.
func (*Store) LockUserRepos(ctx context.Context, h db.Handler, userID int64) error {
	switch driver := h.DriverName(); driver {
	case "postgres":
		_, err := h.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE;`, userID)
		return err
	case "sqlite":
		return nil
	default:
		return fmt.Errorf("lock user repos: unsupported database driver %q", driver)
	}
}

// GetRepoByName implements store.Store.
func (*Store) GetRepoByName(ctx context.Context, tx db.Handler, name string) (models.Repo, error) {
	var repo models.Repo
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT * FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &repo, query, name)
	return repo, err
}

// GetRepoByID implements store.Store.
func (*Store) GetRepoByID(ctx context.Context, h db.Handler, id int64) (models.Repo, error) {
	var repo models.Repo
	query := h.Rebind("SELECT * FROM repos WHERE id = ?;")
	err := h.GetContext(ctx, &repo, query, id)
	return repo, err
}

// GetRepoByNameForUpdate implements store.Store. See LockUserRepos
// for the driver switch.
func (*Store) GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	var query string
	switch driver := h.DriverName(); driver {
	case "postgres":
		query = "SELECT * FROM repos WHERE name = $1 FOR UPDATE;"
	case "sqlite":
		query = "SELECT * FROM repos WHERE name = ?;"
	default:
		return models.Repo{}, fmt.Errorf("get repo for update: unsupported database driver %q", driver)
	}
	var repo models.Repo
	err := h.GetContext(ctx, &repo, query, utils.SanitizeRepo(name))
	return repo, err
}

// GetRepoDescriptionByName implements store.Store.
func (*Store) GetRepoDescriptionByName(ctx context.Context, tx db.Handler, name string) (string, error) {
	var description string
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT description FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &description, query, name)
	return description, err
}

// GetRepoIsHiddenByName implements store.Store.
func (*Store) GetRepoIsHiddenByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isHidden bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT hidden FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isHidden, query, name)
	return isHidden, err
}

// GetRepoIsMirrorByName implements store.Store.
func (*Store) GetRepoIsMirrorByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isMirror bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT mirror FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isMirror, query, name)
	return isMirror, err
}

// SetRepoIsMirrorByName implements store.Store.
func (*Store) SetRepoIsMirrorByName(ctx context.Context, tx db.Handler, name string, isMirror bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET mirror = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, isMirror, name))
}

// SetRepoMirrorSyncByName implements store.Store. It leaves
// updated_at alone: a sync is not a change made to the repository's settings.
//
// The error is text git and the remote wrote, so it is stored as valid UTF-8
// without NUL bytes, which Postgres refuses in a TEXT column: bytes that are
// not UTF-8 become U+FFFD and NULs are dropped.
func (*Store) SetRepoMirrorSyncByName(ctx context.Context, tx db.Handler, name string, syncErr string) error {
	name = utils.SanitizeRepo(name)
	query := "UPDATE repos SET mirror_error = ?, mirror_synced_at = CURRENT_TIMESTAMP WHERE name = ?;"
	if syncErr != "" {
		query = "UPDATE repos SET mirror_error = ? WHERE name = ?;"
		syncErr = utils.StorableText(syncErr)
		if syncErr == "" {
			// Still a failure, though nothing of its text was kept.
			syncErr = "\uFFFD"
		}
	}
	_, err := tx.ExecContext(ctx, tx.Rebind(query), syncErr, name)
	return err
}

// GetRepoIsPrivateByName implements store.Store.
func (*Store) GetRepoIsPrivateByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isPrivate bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT private FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isPrivate, query, name)
	return isPrivate, err
}

// GetRepoProjectNameByName implements store.Store.
func (*Store) GetRepoProjectNameByName(ctx context.Context, tx db.Handler, name string) (string, error) {
	var pname string
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT project_name FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &pname, query, name)
	return pname, err
}

// SetRepoDescriptionByName implements store.Store.
func (*Store) SetRepoDescriptionByName(ctx context.Context, tx db.Handler, name string, description string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET description = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, description, name))
}

// SetRepoIsHiddenByName implements store.Store.
func (*Store) SetRepoIsHiddenByName(ctx context.Context, tx db.Handler, name string, isHidden bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET hidden = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, isHidden, name))
}

// SetRepoIsPrivateByName implements store.Store.
func (*Store) SetRepoIsPrivateByName(ctx context.Context, tx db.Handler, name string, isPrivate bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET private = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, isPrivate, name))
}

// SetRepoNameByName implements store.Store.
func (*Store) SetRepoNameByName(ctx context.Context, tx db.Handler, name string, newName string) error {
	name = utils.SanitizeRepo(name)
	newName = utils.SanitizeRepo(newName)
	query := tx.Rebind("UPDATE repos SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, newName, name))
}

// SetRepoProjectNameByName implements store.Store.
func (*Store) SetRepoProjectNameByName(ctx context.Context, tx db.Handler, name string, projectName string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET project_name = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(tx.ExecContext(ctx, query, projectName, name))
}
