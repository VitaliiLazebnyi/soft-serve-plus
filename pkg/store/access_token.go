package store

import (
	"context"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// AccessTokenStore is an interface for managing access tokens.
type AccessTokenStore interface {
	GetAccessTokenByToken(ctx context.Context, h db.Handler, token string) (models.AccessToken, error)
	GetAccessTokensByUserID(ctx context.Context, h db.Handler, userID int64) ([]models.AccessToken, error)
	CreateAccessToken(ctx context.Context, h db.Handler, name string, userID int64, token string, expiresAt time.Time) (models.AccessToken, error)
	// DeleteAccessTokenForUser deletes the user's token with the given ID. It
	// returns db.ErrRecordNotFound when the user has no such token.
	DeleteAccessTokenForUser(ctx context.Context, h db.Handler, userID int64, id int64) error
}
