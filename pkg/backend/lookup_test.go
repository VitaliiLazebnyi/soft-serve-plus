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
)

// TestIsCollaboratorNotCollaborator verifies that a user who is not a
// collaborator is reported as such without an error, so callers do not have
// to tell a missing row apart from a failed lookup.
func TestIsCollaboratorNotCollaborator(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "collab", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	level, isCollab, err := collabOf(t, be, ctx, "repo", "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if isCollab {
		t.Fatal("not true: !isCollab")
	}
	if got, want := level, access.Level(-1); got != want {
		t.Fatalf("level = %v, want %v", got, want)
	}

	// A repository that does not exist has no collaborators either.
	level, isCollab, err = collabOf(t, be, ctx, "missing", "collab")
	if err != nil {
		t.Fatal(err)
	}
	if isCollab {
		t.Fatal("not true: !isCollab")
	}
	if got, want := level, access.Level(-1); got != want {
		t.Fatalf("level = %v, want %v", got, want)
	}

	level, isCollab, err = collabOf(t, be, ctx, "repo", "collab")
	if err != nil {
		t.Fatal(err)
	}
	if !isCollab {
		t.Fatal("not true: isCollab")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}
}

// TestUserByAccessToken verifies the token lookup finds the token's owner,
// refuses an expired token and reports an unknown one as a missing user.
func TestUserByAccessToken(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateUser(ctx, "other", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := be.CreateAccessToken(ctx, owner, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	u, err := be.UserByAccessToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.ID(), owner.ID(); got != want {
		t.Fatalf("u.ID() = %v, want %v", got, want)
	}
	if got := u.Username(); got != "owner" {
		t.Fatalf("u.Username() = %v, want %v", got, "owner")
	}

	expired, err := be.CreateAccessToken(ctx, owner, "old", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.UserByAccessToken(ctx, expired)
	if !errors.Is(err, proto.ErrTokenExpired) {
		t.Fatal("not true: errors.Is(err, proto.ErrTokenExpired)")
	}

	_, err = be.UserByAccessToken(ctx, "not-a-token")
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
}

// TestSettingsLookupFailureIsLogged verifies that when the settings cannot
// be read, AnonAccess and AllowKeyless still fail closed and log why.
func TestSettingsLookupFailureIsLogged(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	if err := be.db.Close(); err != nil {
		t.Fatal(err)
	}

	if got := be.AnonAccess(ctx); got != access.NoAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.NoAccess)
	}
	if got := be.AllowKeyless(ctx); got != false {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, false)
	}

	out := logs.String()
	if !strings.Contains(out, "error getting anon-access setting") {
		t.Fatal("not true: strings.Contains(out, \"error getting anon-access setting\")")
	}
	if !strings.Contains(out, "error getting allow-keyless setting") {
		t.Fatal("not true: strings.Contains(out, \"error getting allow-keyless setting\")")
	}
}

// An SSH session resolves its user once. If that account is renamed and
// another account takes the old name, the session keeps its own account's
// collaborator rows and gets nothing from the other account's: Readable
// looks collaborators up by user ID, as ReadableRepositories does.
func TestCollaboratorLookedUpByUserID(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	session, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.SetUsername(ctx, "alice", "carol"); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "alice", "private", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	r, level := noLookupError(t)(be.Readable(ctx, "private", session))
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	repos, err := be.ReadableRepositories(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 0 {
		t.Fatalf("len(repos) = %v, want %v", got, 0)
	}

	// The account that holds the name now has the row.
	alice, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	r, level = noLookupError(t)(be.Readable(ctx, "private", alice))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}
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
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()

	admin := defaultAdmin(t, be)
	_, err := be.CreateRepository(ctx, "private", admin, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Without the collaborators table, the stranger's level is unknown.
	breakTable(t, be, "collabs")
	for _, readable := range []func(context.Context, string, proto.User) (proto.Repository, access.Level, error){be.Readable, be.ReadableForUser} {
		r, level, err := readable(ctx, "private", stranger)
		if err == nil {
			t.Fatal("not true: err != nil")
		}
		if r != nil {
			t.Fatal("not true: r == nil")
		}
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
	}
	// The repository and the caller's collaborator row are read in one
	// query, so the lookup fails for its owner and admins too.
	r, level, err := be.Readable(ctx, "private", admin)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}

	// So it does without the repositories table.
	breakTable(t, be, "repos")
	r, level, err = be.ReadableForUser(ctx, "private", admin)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	_, err = be.ReadableRepositoriesForUser(ctx, stranger)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	if strings.Contains(logs.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"ERRO\")")
	}
}

// The user and token lookups leave logging a failure to their callers,
// which know whether the client went away.
func TestUserLookupErrorsNotLogged(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	u, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, u, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	breakTable(t, be, "users")
	_, err = be.User(ctx, "alice")
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByID(ctx, u.ID())
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByPublicKey(ctx, newTestPublicKey(t))
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByAccessToken(ctx, token)
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}

	if strings.Contains(logs.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"ERRO\")")
	}
}
