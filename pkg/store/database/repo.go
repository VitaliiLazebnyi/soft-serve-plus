package database

import (
	"context"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

type repoStore struct{}

var _ store.RepositoryStore = (*repoStore)(nil)

// CreateRepo implements store.RepositoryStore.
func (*repoStore) CreateRepo(ctx context.Context, tx db.Handler, name string, userID int64, projectName string, description string, isPrivate bool, isHidden bool, isMirror bool, isAnonymous bool) error {
	name = utils.SanitizeRepo(name)
	values := []interface{}{
		name, projectName, description, isPrivate, isMirror, isHidden, isAnonymous,
	}
	query := `INSERT INTO repos (name, project_name, description, private, mirror, hidden, anonymous, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP);`
	if userID > 0 {
		query = `INSERT INTO repos (name, project_name, description, private, mirror, hidden, anonymous, updated_at, user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?);`
		values = append(values, userID)
	}

	query = tx.Rebind(query)
	_, err := tx.ExecContext(ctx, query, values...)
	return err
}

// DeleteRepoByName implements store.RepositoryStore.
func (*repoStore) DeleteRepoByName(ctx context.Context, tx db.Handler, name string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("DELETE FROM repos WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, name)
	return err
}

// GetAllRepos implements store.RepositoryStore.
func (*repoStore) GetAllRepos(ctx context.Context, tx db.Handler) ([]models.Repo, error) {
	var repos []models.Repo
	query := tx.Rebind("SELECT * FROM repos ORDER BY name;")
	err := tx.SelectContext(ctx, &repos, query)
	return repos, err
}

// GetUserRepos implements store.RepositoryStore.
func (*repoStore) GetUserRepos(ctx context.Context, tx db.Handler, userID int64) ([]models.Repo, error) {
	var repos []models.Repo
	query := tx.Rebind("SELECT * FROM repos WHERE user_id = ? ORDER BY name;")
	err := tx.SelectContext(ctx, &repos, query, userID)
	return repos, err
}

// CountUserRepos implements store.RepositoryStore.
func (*repoStore) CountUserRepos(ctx context.Context, tx db.Handler, userID int64) (int64, error) {
	var n int64
	query := tx.Rebind("SELECT COUNT(*) FROM repos WHERE user_id = ? AND anonymous = ?;")
	err := tx.GetContext(ctx, &n, query, userID, false)
	return n, err
}

// CountAnonymousRepos implements store.RepositoryStore.
func (*repoStore) CountAnonymousRepos(ctx context.Context, tx db.Handler) (int64, error) {
	var n int64
	query := tx.Rebind("SELECT COUNT(*) FROM repos WHERE anonymous = ?;")
	err := tx.GetContext(ctx, &n, query, true)
	return n, err
}

// LockUserRepos implements store.RepositoryStore. See LockAndCountWebhooks
// for why FOR NO KEY UPDATE: it does not block the key-share lock that
// inserting a repository owned by the user takes.
func (*repoStore) LockUserRepos(ctx context.Context, h db.Handler, userID int64) error {
	switch driver := h.DriverName(); driver {
	case "postgres":
		_, err := h.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE;`, userID)
		return err
	case "sqlite", "sqlite3":
		return nil
	default:
		return fmt.Errorf("lock user repos: unsupported database driver %q", driver)
	}
}

// GetRepoByName implements store.RepositoryStore.
func (*repoStore) GetRepoByName(ctx context.Context, tx db.Handler, name string) (models.Repo, error) {
	var repo models.Repo
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT * FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &repo, query, name)
	return repo, err
}

// GetRepoByID implements store.RepositoryStore.
func (*repoStore) GetRepoByID(ctx context.Context, h db.Handler, id int64) (models.Repo, error) {
	var repo models.Repo
	query := h.Rebind("SELECT * FROM repos WHERE id = ?;")
	err := h.GetContext(ctx, &repo, query, id)
	return repo, err
}

// GetRepoByNameForUpdate implements store.RepositoryStore. See LockUserRepos
// for the driver switch.
func (*repoStore) GetRepoByNameForUpdate(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	var query string
	switch driver := h.DriverName(); driver {
	case "postgres":
		query = "SELECT * FROM repos WHERE name = $1 FOR UPDATE;"
	case "sqlite", "sqlite3":
		query = "SELECT * FROM repos WHERE name = ?;"
	default:
		return models.Repo{}, fmt.Errorf("get repo for update: unsupported database driver %q", driver)
	}
	var repo models.Repo
	err := h.GetContext(ctx, &repo, query, utils.SanitizeRepo(name))
	return repo, err
}

// GetRepoDescriptionByName implements store.RepositoryStore.
func (*repoStore) GetRepoDescriptionByName(ctx context.Context, tx db.Handler, name string) (string, error) {
	var description string
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT description FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &description, query, name)
	return description, err
}

// GetRepoIsHiddenByName implements store.RepositoryStore.
func (*repoStore) GetRepoIsHiddenByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isHidden bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT hidden FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isHidden, query, name)
	return isHidden, err
}

// GetRepoIsMirrorByName implements store.RepositoryStore.
func (*repoStore) GetRepoIsMirrorByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isMirror bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT mirror FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isMirror, query, name)
	return isMirror, err
}

// SetRepoIsMirrorByName implements store.RepositoryStore.
func (*repoStore) SetRepoIsMirrorByName(ctx context.Context, tx db.Handler, name string, isMirror bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET mirror = ? WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, isMirror, name)
	return err
}

// GetRepoIsPrivateByName implements store.RepositoryStore.
func (*repoStore) GetRepoIsPrivateByName(ctx context.Context, tx db.Handler, name string) (bool, error) {
	var isPrivate bool
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT private FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &isPrivate, query, name)
	return isPrivate, err
}

// GetRepoProjectNameByName implements store.RepositoryStore.
func (*repoStore) GetRepoProjectNameByName(ctx context.Context, tx db.Handler, name string) (string, error) {
	var pname string
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("SELECT project_name FROM repos WHERE name = ?;")
	err := tx.GetContext(ctx, &pname, query, name)
	return pname, err
}

// SetRepoDescriptionByName implements store.RepositoryStore.
func (*repoStore) SetRepoDescriptionByName(ctx context.Context, tx db.Handler, name string, description string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET description = ? WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, description, name)
	return err
}

// SetRepoIsHiddenByName implements store.RepositoryStore.
func (*repoStore) SetRepoIsHiddenByName(ctx context.Context, tx db.Handler, name string, isHidden bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET hidden = ? WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, isHidden, name)
	return err
}

// SetRepoIsPrivateByName implements store.RepositoryStore.
func (*repoStore) SetRepoIsPrivateByName(ctx context.Context, tx db.Handler, name string, isPrivate bool) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET private = ? WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, isPrivate, name)
	return err
}

// SetRepoNameByName implements store.RepositoryStore.
func (*repoStore) SetRepoNameByName(ctx context.Context, tx db.Handler, name string, newName string) error {
	name = utils.SanitizeRepo(name)
	newName = utils.SanitizeRepo(newName)
	query := tx.Rebind("UPDATE repos SET name = ? WHERE name = ?;")
	res, err := tx.ExecContext(ctx, query, newName, name)
	if err != nil {
		return err
	}

	// Nothing updated means there was no such repository.
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrRecordNotFound
	}
	return nil
}

// SetRepoProjectNameByName implements store.RepositoryStore.
func (*repoStore) SetRepoProjectNameByName(ctx context.Context, tx db.Handler, name string, projectName string) error {
	name = utils.SanitizeRepo(name)
	query := tx.Rebind("UPDATE repos SET project_name = ? WHERE name = ?;")
	_, err := tx.ExecContext(ctx, query, projectName, name)
	return err
}
