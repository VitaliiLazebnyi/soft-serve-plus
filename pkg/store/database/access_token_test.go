package database

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

func TestAccessTokenStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	if err := s.CreateUser(ctx, dbx, "other", false, nil); err != nil {
		t.Fatal(err)
	}
	other, err := s.FindUserByUsername(ctx, dbx, "other")
	if err != nil {
		t.Fatal(err)
	}

	forever, err := s.CreateAccessToken(ctx, dbx, "forever", owner, "hash-forever", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if forever.Name != "forever" || forever.UserID != owner || forever.Token != "hash-forever" || forever.ExpiresAt.Valid {
		t.Errorf("CreateAccessToken(no expiry) = %+v", forever)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	expiring, err := s.CreateAccessToken(ctx, dbx, "expiring", owner, "hash-expiring", expires)
	if err != nil {
		t.Fatal(err)
	}
	if !expiring.ExpiresAt.Valid || !expiring.ExpiresAt.Time.Equal(expires) {
		t.Errorf("CreateAccessToken(expiring) expires at %v, want %v", expiring.ExpiresAt, expires)
	}

	if got, err := s.GetAccessTokenByToken(ctx, dbx, "hash-expiring"); err != nil || got.ID != expiring.ID {
		t.Errorf("GetAccessTokenByToken() = %+v, %v, want token %d", got, err, expiring.ID)
	}
	if got, err := s.GetAccessTokenByToken(ctx, dbx, "hash-forever"); err != nil || got.UserID != owner {
		t.Errorf("GetAccessTokenByToken() = %+v, %v, want user %d", got, err, owner)
	}
	if ts, err := s.GetAccessTokensByUserID(ctx, dbx, owner); err != nil || len(ts) != 2 {
		t.Errorf("GetAccessTokensByUserID() = %d, %v, want 2", len(ts), err)
	}

	// A user cannot delete someone else's token, and is told it was not
	// found, exactly like a missing one.
	if err := s.DeleteAccessTokenForUser(ctx, dbx, other.ID, forever.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("DeleteAccessTokenForUser(other user) error = %v, want ErrRecordNotFound", err)
	}
	if _, err := s.GetAccessTokenByToken(ctx, dbx, "hash-forever"); err != nil {
		t.Errorf("token deleted by another user: GetAccessTokenByToken() error = %v", err)
	}
	if err := s.DeleteAccessTokenForUser(ctx, dbx, owner, forever.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccessTokenByToken(ctx, dbx, "hash-forever"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetAccessTokenByToken() after its owner deleted it: error = %v, want ErrRecordNotFound", err)
	}
	if err := s.DeleteAccessTokenForUser(ctx, dbx, owner, forever.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("DeleteAccessTokenForUser(already deleted) error = %v, want ErrRecordNotFound", err)
	}
	if err := s.DeleteAccessTokenForUser(ctx, dbx, owner, expiring.ID); err != nil {
		t.Fatal(err)
	}
	if ts, err := s.GetAccessTokensByUserID(ctx, dbx, owner); err != nil || len(ts) != 0 {
		t.Errorf("GetAccessTokensByUserID() after deleting both = %d, %v, want 0", len(ts), err)
	}
}

// A token ID past the access_tokens.id column's range, which is 32-bit on
// Postgres, names no token instead of failing as out of range.
func TestAccessTokenIDBeyondColumnRange(t *testing.T) {
	t.Parallel()
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	tok, err := s.CreateAccessToken(ctx, dbx, "kept", owner, "hash-kept", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{math.MaxInt32 + 1, math.MaxInt64} {
		if err := s.DeleteAccessTokenForUser(ctx, dbx, owner, id); !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("DeleteAccessTokenForUser(%d) error = %v, want ErrRecordNotFound", id, err)
		}
	}
	if got, err := s.GetAccessTokenByToken(ctx, dbx, "hash-kept"); err != nil || got.ID != tok.ID {
		t.Errorf("GetAccessTokenByToken() = %+v, %v, want token %d", got, err, tok.ID)
	}
}
