package database

import (
	"context"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// CreateAccessToken implements store.Store.
func (*Store) CreateAccessToken(ctx context.Context, h db.Handler, name string, userID int64, token string, expiresAt time.Time) (models.AccessToken, error) {
	queryWithoutExpires := `INSERT INTO access_tokens (name, user_id, token, created_at, updated_at)
	VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING ` + accessTokenColumns
	queryWithExpires := `INSERT INTO access_tokens (name, user_id, token, expires_at, created_at, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING ` + accessTokenColumns

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

// DeleteAccessTokenForUser implements store.Store. The ID comes from the
// user, and access_tokens.id is 32-bit on Postgres, which types a bare
// parameter compared with it as int4 and refuses an ID past 2^31-1; with
// CAST(? AS BIGINT) such an ID matches no token instead.
func (*Store) DeleteAccessTokenForUser(ctx context.Context, h db.Handler, userID int64, id int64) error {
	query := h.Rebind(`DELETE FROM access_tokens WHERE user_id = ? AND id = CAST(? AS BIGINT)`)
	return affected(h.ExecContext(ctx, query, userID, id))
}

// GetAccessTokensByUserID implements store.Store.
func (*Store) GetAccessTokensByUserID(ctx context.Context, h db.Handler, userID int64) ([]models.AccessToken, error) {
	query := h.Rebind(`SELECT ` + accessTokenColumns + ` FROM access_tokens WHERE user_id = ? ORDER BY id`)
	var m []models.AccessToken
	err := h.SelectContext(ctx, &m, query, userID)
	return m, err
}

// GetAccessTokenByToken implements store.Store.
func (*Store) GetAccessTokenByToken(ctx context.Context, h db.Handler, token string) (models.AccessToken, error) {
	query := h.Rebind(`SELECT ` + accessTokenColumns + ` FROM access_tokens WHERE token = ?`)
	var m models.AccessToken
	err := h.GetContext(ctx, &m, query, token)
	return m, err
}
