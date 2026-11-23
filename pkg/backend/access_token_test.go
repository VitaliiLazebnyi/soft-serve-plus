package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestDeleteAccessTokenOtherUser verifies that deleting another user's token
// fails exactly like deleting a missing one and leaves the token in place.
func TestDeleteAccessTokenOtherUser(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := be.CreateUser(ctx, "other", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateAccessToken(ctx, owner, "t", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := be.ListAccessTokens(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tokens); got != 1 {
		t.Fatalf("len(tokens) = %v, want %v", got, 1)
	}
	id := tokens[0].ID

	err = be.DeleteAccessToken(ctx, other, id)
	if !errors.Is(err, proto.ErrTokenNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrTokenNotFound)")
	}
	err = be.DeleteAccessToken(ctx, other, id+1000)
	if !errors.Is(err, proto.ErrTokenNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrTokenNotFound)")
	}

	tokens, err = be.ListAccessTokens(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tokens); got != 1 {
		t.Fatalf("len(tokens) = %v, want %v", got, 1)
	}

	if err := be.DeleteAccessToken(ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	err = be.DeleteAccessToken(ctx, owner, id)
	if !errors.Is(err, proto.ErrTokenNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrTokenNotFound)")
	}
}
