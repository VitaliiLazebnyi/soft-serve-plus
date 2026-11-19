package cmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
)

// breakTable renames table so every query on it fails, as a database
// fault would.
func breakTable(t *testing.T, ctx context.Context, table string) {
	t.Helper()
	dbx := db.FromContext(ctx)
	if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatal(err)
	}
}

// `repo list` lists the repositories of the session's user, as the session
// resolved it. A lookup of the session's key that fails no longer turns the
// list into what an anonymous caller sees.
func TestRepoListUsesSessionUser(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	is.NoErr(err)
	signer, err := gossh.NewSignerFromKey(priv)
	is.NoErr(err)
	pk := signer.PublicKey()
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "mine", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	sessCtx := context.WithValue(proto.WithUserContext(ctx, owner), ssh.ContextKeyPublicKey, pk)

	// Key lookups now fail; the session's user was resolved before.
	breakTable(t, ctx, "public_keys")

	out, _, err := runRepoOutput(t, sessCtx, "list")
	is.NoErr(err)
	is.True(strings.Contains(out, "mine"))
}

// A repository lookup that fails is answered as a server fault, not as a
// missing repository, and the cause is kept from the client.
func TestRepoCommandLookupFailure(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "repo", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	strangerCtx := withUser(t, ctx, be, "stranger", false)

	is.True(errors.Is(runRepo(t, strangerCtx, "info", "repo"), proto.ErrRepoNotFound))

	// Without the collaborators table, the stranger's level cannot be
	// worked out.
	breakTable(t, ctx, "collabs")
	err = runRepo(t, strangerCtx, "info", "repo")
	is.True(errors.Is(err, errLookupFailed))
	is.True(!strings.Contains(err.Error(), "collabs"))

	// With the repository table gone, the lookup fails for everyone, and
	// so does the list.
	breakTable(t, ctx, "repos")
	is.True(errors.Is(runRepo(t, ownerCtx, "info", "repo"), errLookupFailed))
	is.True(errors.Is(runRepo(t, ownerCtx, "list"), errLookupFailed))
}
