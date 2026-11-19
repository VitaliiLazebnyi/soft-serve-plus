package cmd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// An SSH session resolves its user once. If that account is renamed and
// another account takes the old name, the session's account commands still
// act on the session's own account: `pubkey add` must not give the other
// account the key (which would let the key's holder sign in as it), `pubkey
// remove` must not remove the other account's keys, and `set-username`
// must not rename the other account.
func TestAccountCommandsActOnSessionAccountAfterRename(t *testing.T) {
	is := is.New(t)
	newKey := func() gossh.PublicKey {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		is.NoErr(err)
		signer, err := gossh.NewSignerFromKey(priv)
		is.NoErr(err)
		return signer.PublicKey()
	}
	run := func(ctx context.Context, c *cobra.Command, args ...string) error {
		c.SilenceUsage = true
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		return c.ExecuteContext(ctx)
	}
	ctx, be := newAuthTestContext(t)
	keysOf := func(ctx context.Context, name string) []gossh.PublicKey {
		t.Helper()
		u, err := be.User(ctx, name)
		is.NoErr(err)
		return u.PublicKeys()
	}
	k1, k2 := newKey(), newKey()
	session, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{k1}})
	is.NoErr(err)
	sessCtx := context.WithValue(proto.WithUserContext(ctx, session), ssh.ContextKeyPublicKey, k1)

	// An admin renames the account; another account takes the old name.
	is.NoErr(be.SetUsername(ctx, "alice", "carol"))
	// The other account has two keys, so removing one of them is not
	// refused as its last key: only the account decides.
	mk, mk2 := newKey(), newKey()
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{mk, mk2}})
	is.NoErr(err)

	is.NoErr(run(sessCtx, PubkeyCommand(), "add", sshutils.MarshalAuthorizedKey(k2)))
	is.Equal(len(keysOf(ctx, "carol")), 2)
	is.Equal(len(keysOf(ctx, "alice")), 2)

	// The other account's key is not the session's to remove.
	err = run(sessCtx, PubkeyCommand(), "remove", sshutils.MarshalAuthorizedKey(mk))
	is.True(errors.Is(err, proto.ErrPublicKeyNotFound))
	is.Equal(len(keysOf(ctx, "alice")), 2)
	is.NoErr(run(sessCtx, PubkeyCommand(), "remove", sshutils.MarshalAuthorizedKey(k2)))
	is.Equal(len(keysOf(ctx, "carol")), 1)

	is.NoErr(run(sessCtx, SetUsernameCommand(), "dave"))
	_, err = be.User(ctx, "dave")
	is.NoErr(err)
	_, err = be.User(ctx, "carol")
	is.True(errors.Is(err, proto.ErrUserNotFound))
	is.Equal(len(keysOf(ctx, "alice")), 2) // the other account kept its name and keys
}
