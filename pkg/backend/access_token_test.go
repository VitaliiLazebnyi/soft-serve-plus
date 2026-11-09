package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestDeleteAccessTokenOtherUser verifies that deleting another user's token
// fails exactly like deleting a missing one and leaves the token in place.
func TestDeleteAccessTokenOtherUser(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	other, err := be.CreateUser(ctx, "other", proto.UserOptions{})
	is.NoErr(err)

	_, err = be.CreateAccessToken(ctx, owner, "t", time.Time{})
	is.NoErr(err)
	tokens, err := be.ListAccessTokens(ctx, owner)
	is.NoErr(err)
	is.Equal(len(tokens), 1)
	id := tokens[0].ID

	err = be.DeleteAccessToken(ctx, other, id)
	is.True(errors.Is(err, proto.ErrTokenNotFound))
	err = be.DeleteAccessToken(ctx, other, id+1000)
	is.True(errors.Is(err, proto.ErrTokenNotFound))

	tokens, err = be.ListAccessTokens(ctx, owner)
	is.NoErr(err)
	is.Equal(len(tokens), 1)

	is.NoErr(be.DeleteAccessToken(ctx, owner, id))
	err = be.DeleteAccessToken(ctx, owner, id)
	is.True(errors.Is(err, proto.ErrTokenNotFound))
}
