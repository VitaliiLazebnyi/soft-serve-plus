package backend

import (
	"context"
	"errors"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// CreateAccessToken creates an access token for user.
func (d *Backend) CreateAccessToken(ctx context.Context, user proto.User, name string, expiresAt time.Time) (string, error) {
	token := GenerateToken()
	tokenHash := HashToken(token)
	name = utils.Sanitize(name)

	if err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := d.store.CreateAccessToken(ctx, tx, name, user.ID(), tokenHash, expiresAt)
		if err != nil {
			return err
		}

		return nil
	}); err != nil {
		return "", err
	}

	return token, nil
}

// DeleteAccessToken deletes an access token for a user.
func (d *Backend) DeleteAccessToken(ctx context.Context, user proto.User, id int64) error {
	err := d.db.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := d.store.GetAccessToken(ctx, tx, id)
		if err != nil {
			return err
		}

		if err := d.store.DeleteAccessTokenForUser(ctx, tx, user.ID(), id); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, db.ErrRecordNotFound) {
			return proto.ErrTokenNotFound
		}
		return err
	}

	return nil
}

// ListAccessTokens lists access tokens for a user.
func (d *Backend) ListAccessTokens(ctx context.Context, user proto.User) ([]proto.AccessToken, error) {
	accessTokens, err := d.store.GetAccessTokensByUserID(ctx, d.db, user.ID())
	if err != nil {
		return nil, err
	}

	var tokens []proto.AccessToken
	for _, t := range accessTokens {
		token := proto.AccessToken{
			ID:        t.ID,
			Name:      t.Name,
			TokenHash: t.Token,
			UserID:    t.UserID,
			CreatedAt: t.CreatedAt,
		}
		if t.ExpiresAt.Valid {
			token.ExpiresAt = t.ExpiresAt.Time
		}

		tokens = append(tokens, token)
	}

	return tokens, nil
}
