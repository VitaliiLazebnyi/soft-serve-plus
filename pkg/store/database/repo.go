package database

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// CreateRepo implements store.Store.
func (*Store) CreateRepo(ctx context.Context, h db.Handler, r models.Repo) (models.Repo, error) {
	if r.UserID <= 0 {
		// repos.user_id is NOT NULL and references users.
		return models.Repo{}, fmt.Errorf("create repo %q: invalid owner user ID %d", r.Name, r.UserID)
	}
	var m models.Repo
	query := h.Rebind(`INSERT INTO repos (name, project_name, description, private, mirror, hidden, anonymous, updated_at, user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
			RETURNING *;`)
	err := h.GetContext(ctx, &m, query, utils.SanitizeRepo(r.Name), r.ProjectName, r.Description, r.Private, r.Mirror, r.Hidden, r.Anonymous, r.UserID)
	return m, err
}

// DeleteRepoByName implements store.Store.
func (*Store) DeleteRepoByName(ctx context.Context, h db.Handler, name string) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("DELETE FROM repos WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, name))
}

// GetAllRepos implements store.Store.
func (*Store) GetAllRepos(ctx context.Context, h db.Handler) ([]models.Repo, error) {
	var repos []models.Repo
	query := h.Rebind("SELECT * FROM repos ORDER BY name;")
	err := h.SelectContext(ctx, &repos, query)
	return repos, err
}

// GetUserRepos implements store.Store.
func (*Store) GetUserRepos(ctx context.Context, h db.Handler, userID int64) ([]models.Repo, error) {
	var repos []models.Repo
	query := h.Rebind("SELECT * FROM repos WHERE user_id = ? ORDER BY name;")
	err := h.SelectContext(ctx, &repos, query, userID)
	return repos, err
}

// CountUserRepos implements store.Store.
func (*Store) CountUserRepos(ctx context.Context, h db.Handler, userID int64) (int64, error) {
	var n int64
	query := h.Rebind("SELECT COUNT(*) FROM repos WHERE user_id = ? AND anonymous = ?;")
	err := h.GetContext(ctx, &n, query, userID, false)
	return n, err
}

// CountAnonymousRepos implements store.Store.
func (*Store) CountAnonymousRepos(ctx context.Context, h db.Handler) (int64, error) {
	var n int64
	query := h.Rebind("SELECT COUNT(*) FROM repos WHERE anonymous = ?;")
	err := h.GetContext(ctx, &n, query, true)
	return n, err
}

// LockUserRepos implements store.Store. See LockAndCountWebhooks
// for why FOR NO KEY UPDATE: it does not block the key-share lock that
// inserting a repository owned by the user takes.
func (*Store) LockUserRepos(ctx context.Context, h db.Handler, userID int64) error {
	if h.DriverName() != "postgres" {
		return nil
	}
	_, err := h.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE;`, userID)
	return err
}

// GetRepoByName implements store.Store.
func (*Store) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	var repo models.Repo
	name = utils.SanitizeRepo(name)
	query := h.Rebind("SELECT * FROM repos WHERE name = ?;")
	err := h.GetContext(ctx, &repo, query, name)
	return repo, err
}

// GetRepoByID implements store.Store.
func (*Store) GetRepoByID(ctx context.Context, h db.Handler, id int64) (models.Repo, error) {
	var repo models.Repo
	query := h.Rebind("SELECT * FROM repos WHERE id = ?;")
	err := h.GetContext(ctx, &repo, query, id)
	return repo, err
}

// GetRepoByIDForUpdate implements store.Store. See LockUserRepos for the
// driver switch.
func (*Store) GetRepoByIDForUpdate(ctx context.Context, h db.Handler, id int64) (models.Repo, error) {
	query := "SELECT * FROM repos WHERE id = ?;"
	if h.DriverName() == "postgres" {
		query = "SELECT * FROM repos WHERE id = $1 FOR UPDATE;"
	}
	var repo models.Repo
	err := h.GetContext(ctx, &repo, query, id)
	return repo, err
}

// GetRepoByNameForUpdate implements store.Store. See LockUserRepos
// for the driver switch.
func (*Store) GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	query := "SELECT * FROM repos WHERE name = ?;"
	if h.DriverName() == "postgres" {
		query = "SELECT * FROM repos WHERE name = $1 FOR UPDATE;"
	}
	var repo models.Repo
	err := h.GetContext(ctx, &repo, query, utils.SanitizeRepo(name))
	return repo, err
}

// SetRepoIsMirrorByName implements store.Store.
func (*Store) SetRepoIsMirrorByName(ctx context.Context, h db.Handler, name string, isMirror bool) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("UPDATE repos SET mirror = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, isMirror, name))
}

// SetRepoMirrorSyncByID implements store.Store. It leaves
// updated_at alone: a sync is not a change made to the repository's settings.
//
// The error is text git and the remote wrote, so it is stored as valid UTF-8
// without NUL bytes, which Postgres refuses in a TEXT column: bytes that are
// not UTF-8 become U+FFFD and NULs are dropped.
func (*Store) SetRepoMirrorSyncByID(ctx context.Context, h db.Handler, id int64, syncErr string) error {
	query := "UPDATE repos SET mirror_error = ?, mirror_synced_at = CURRENT_TIMESTAMP WHERE id = ?;"
	if syncErr != "" {
		query = "UPDATE repos SET mirror_error = ? WHERE id = ?;"
		syncErr = utils.StorableText(syncErr)
		if syncErr == "" {
			// Still a failure, though nothing of its text was kept.
			syncErr = "\uFFFD"
		}
	}
	return affected(h.ExecContext(ctx, h.Rebind(query), syncErr, id))
}

// SetRepoDescriptionByName implements store.Store.
func (*Store) SetRepoDescriptionByName(ctx context.Context, h db.Handler, name string, description string) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("UPDATE repos SET description = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, description, name))
}

// SetRepoIsHiddenByName implements store.Store.
func (*Store) SetRepoIsHiddenByName(ctx context.Context, h db.Handler, name string, isHidden bool) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("UPDATE repos SET hidden = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, isHidden, name))
}

// SetRepoIsPrivateByName implements store.Store.
func (*Store) SetRepoIsPrivateByName(ctx context.Context, h db.Handler, name string, isPrivate bool) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("UPDATE repos SET private = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, isPrivate, name))
}

// SetRepoNameByName implements store.Store.
func (*Store) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	name = utils.SanitizeRepo(name)
	newName = utils.SanitizeRepo(newName)
	query := h.Rebind("UPDATE repos SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, newName, name))
}

// SetRepoProjectNameByName implements store.Store.
func (*Store) SetRepoProjectNameByName(ctx context.Context, h db.Handler, name string, projectName string) error {
	name = utils.SanitizeRepo(name)
	query := h.Rebind("UPDATE repos SET project_name = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?;")
	return affected(h.ExecContext(ctx, query, projectName, name))
}
