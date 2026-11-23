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
	t.Parallel()
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "collab", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadWriteAccess)
	}))

	level, isCollab, err := collabOf(t, be, ctx, "repo", "stranger")
	is.NoErr(err)
	is.True(!isCollab)
	is.Equal(level, access.Level(-1))

	// A repository that does not exist has no collaborators either.
	level, isCollab, err = collabOf(t, be, ctx, "missing", "collab")
	is.NoErr(err)
	is.True(!isCollab)
	is.Equal(level, access.Level(-1))

	level, isCollab, err = collabOf(t, be, ctx, "repo", "collab")
	is.NoErr(err)
	is.True(isCollab)
	is.Equal(level, access.ReadWriteAccess)
}

// TestUserByAccessToken verifies the token lookup finds the token's owner,
// refuses an expired token and reports an unknown one as a missing user.
func TestUserByAccessToken(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// An SSH session resolves its user once. If that account is renamed and
// another account takes the old name, the session keeps its own account's
// collaborator rows and gets nothing from the other account's: Readable
// looks collaborators up by user ID, as ReadableRepositories does.
func TestCollaboratorLookedUpByUserID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	session, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.SetUsername(ctx, "alice", "carol"))
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "alice", "private", access.ReadWriteAccess)
	}))

	r, level := noLookupError(t)(be.Readable(ctx, "private", session))
	is.True(r == nil)
	is.Equal(level, access.NoAccess)
	repos, err := be.ReadableRepositories(ctx, session)
	is.NoErr(err)
	is.Equal(len(repos), 0)

	// The account that holds the name now has the row.
	alice, err := be.User(ctx, "alice")
	is.NoErr(err)
	r, level = noLookupError(t)(be.Readable(ctx, "private", alice))
	is.True(r != nil)
	is.Equal(level, access.ReadWriteAccess)
}

// breakTable renames table so every query on it fails, as a database fault
// would.
func breakTable(t *testing.T, be *Backend, table string) {
	t.Helper()
	if _, err := be.db.ExecContext(context.Background(), "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatal(err)
	}
}

// A lookup that fails is returned, not answered as a missing or unreadable
// repository: Readable and ReadableForUser give the error with no
// repository and no access, and leave logging it to the caller.
func TestReadableReturnsLookupErrors(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()

	admin := defaultAdmin(t, be)
	_, err := be.CreateRepository(ctx, "private", admin, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)

	// Without the collaborators table, the stranger's level is unknown.
	breakTable(t, be, "collabs")
	for _, readable := range []func(context.Context, string, proto.User) (proto.Repository, access.Level, error){be.Readable, be.ReadableForUser} {
		r, level, err := readable(ctx, "private", stranger)
		is.True(err != nil)
		is.True(r == nil)
		is.Equal(level, access.NoAccess)
	}
	// The repository and the caller's collaborator row are read in one
	// query, so the lookup fails for its owner and admins too.
	r, level, err := be.Readable(ctx, "private", admin)
	is.True(err != nil)
	is.True(r == nil)
	is.Equal(level, access.NoAccess)

	// So it does without the repositories table.
	breakTable(t, be, "repos")
	r, level, err = be.ReadableForUser(ctx, "private", admin)
	is.True(err != nil)
	is.True(r == nil)
	is.Equal(level, access.NoAccess)
	_, err = be.ReadableRepositoriesForUser(ctx, stranger)
	is.True(err != nil)

	is.True(!strings.Contains(logs.String(), "ERRO"))
}

// The user and token lookups leave logging a failure to their callers,
// which know whether the client went away (item 259).
func TestUserLookupErrorsNotLogged(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	u, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	token, err := be.CreateAccessToken(ctx, u, "t", time.Now().Add(time.Hour))
	is.NoErr(err)

	breakTable(t, be, "users")
	_, err = be.User(ctx, "alice")
	is.True(err != nil && !errors.Is(err, proto.ErrUserNotFound))
	_, err = be.UserByID(ctx, u.ID())
	is.True(err != nil && !errors.Is(err, proto.ErrUserNotFound))
	_, err = be.UserByPublicKey(ctx, newTestPublicKey(t))
	is.True(err != nil && !errors.Is(err, proto.ErrUserNotFound))
	_, err = be.UserByAccessToken(ctx, token)
	is.True(err != nil && !errors.Is(err, proto.ErrUserNotFound))

	is.True(!strings.Contains(logs.String(), "ERRO"))
}
