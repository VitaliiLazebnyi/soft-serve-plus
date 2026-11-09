package backend

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestIsCollaboratorNotCollaborator verifies that a user who is not a
// collaborator is reported as such without an error, so callers do not have
// to tell a missing row apart from a failed lookup.
func TestIsCollaboratorNotCollaborator(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "collab", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadWriteAccess)
	}))

	level, isCollab, err := be.IsCollaborator(ctx, "repo", "stranger")
	is.NoErr(err)
	is.True(!isCollab)
	is.Equal(level, access.AccessLevel(-1))

	// A repository that does not exist has no collaborators either.
	level, isCollab, err = be.IsCollaborator(ctx, "missing", "collab")
	is.NoErr(err)
	is.True(!isCollab)
	is.Equal(level, access.AccessLevel(-1))

	level, isCollab, err = be.IsCollaborator(ctx, "repo", "collab")
	is.NoErr(err)
	is.True(isCollab)
	is.Equal(level, access.ReadWriteAccess)
}

// TestUserByAccessToken verifies the token lookup finds the token's owner,
// refuses an expired token and reports an unknown one as a missing user.
func TestUserByAccessToken(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateUser(ctx, "other", proto.UserOptions{})
	is.NoErr(err)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)

	tok, err := be.CreateAccessToken(ctx, owner, "t", time.Now().Add(time.Hour))
	is.NoErr(err)
	u, err := be.UserByAccessToken(ctx, tok)
	is.NoErr(err)
	is.Equal(u.ID(), owner.ID())
	is.Equal(u.Username(), "owner")

	expired, err := be.CreateAccessToken(ctx, owner, "old", time.Now().Add(-time.Hour))
	is.NoErr(err)
	_, err = be.UserByAccessToken(ctx, expired)
	is.True(errors.Is(err, proto.ErrTokenExpired))

	_, err = be.UserByAccessToken(ctx, "not-a-token")
	is.True(errors.Is(err, proto.ErrUserNotFound))
}

// TestSettingsLookupFailureIsLogged verifies that when the settings cannot
// be read, AnonAccess and AllowKeyless still fail closed and log why.
func TestSettingsLookupFailureIsLogged(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	is.NoErr(be.db.Close())

	is.Equal(be.AnonAccess(ctx), access.NoAccess)
	is.Equal(be.AllowKeyless(ctx), false)

	out := logs.String()
	is.True(strings.Contains(out, "error getting anon-access setting"))
	is.True(strings.Contains(out, "error getting allow-keyless setting"))
}
