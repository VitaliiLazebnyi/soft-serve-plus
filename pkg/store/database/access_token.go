package database

import (
	"context"
	"database/sql"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// CreateAccessToken implements store.Store.
func (*Store) CreateAccessToken(ctx context.Context, h db.Handler, name string, userID int64, token string, expiresAt time.Time) (models.AccessToken, error) {
	queryWithoutExpires := `INSERT INTO access_tokens (name, user_id, token, created_at, updated_at)
	VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING *`
	queryWithExpires := `INSERT INTO access_tokens (name, user_id, token, expires_at, created_at, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING *`

	query := queryWithoutExpires
	values := []interface{}{name, userID, token}
	if !expiresAt.IsZero() {
		query = queryWithExpires
		values = append(values, expiresAt.UTC())
	}

	var t models.AccessToken
	err := h.GetContext(ctx, &t, h.Rebind(query), values...)
	return t, err
}

// DeleteAccessToken implements store.Store.
func (*Store) DeleteAccessToken(ctx context.Context, h db.Handler, id int64) error {
	query := h.Rebind(`DELETE FROM access_tokens WHERE id = ?`)
	_, err := h.ExecContext(ctx, query, id)
	return err
}

// DeleteAccessTokenForUser implements store.Store.
func (*Store) DeleteAccessTokenForUser(ctx context.Context, h db.Handler, userID int64, id int64) error {
	query := h.Rebind(`DELETE FROM access_tokens WHERE user_id = ? AND id = ?`)
	res, err := h.ExecContext(ctx, query, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetAccessToken implements store.Store.
func (*Store) GetAccessToken(ctx context.Context, h db.Handler, id int64) (models.AccessToken, error) {
	query := h.Rebind(`SELECT * FROM access_tokens WHERE id = ?`)
	var m models.AccessToken
	err := h.GetContext(ctx, &m, query, id)
	return m, err
}

// GetAccessTokensByUserID implements store.Store.
func (*Store) GetAccessTokensByUserID(ctx context.Context, h db.Handler, userID int64) ([]models.AccessToken, error) {
	query := h.Rebind(`SELECT * FROM access_tokens WHERE user_id = ? ORDER BY id`)
	var m []models.AccessToken
	err := h.SelectContext(ctx, &m, query, userID)
	return m, err
}

// GetAccessTokenByToken implements store.Store.
func (*Store) GetAccessTokenByToken(ctx context.Context, h db.Handler, token string) (models.AccessToken, error) {
	query := h.Rebind(`SELECT * FROM access_tokens WHERE token = ?`)
	var m models.AccessToken
	err := h.GetContext(ctx, &m, query, token)
	return m, err
}
