package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
)

// runPubkeyIn runs a `pubkey` subcommand with stdin as its input and returns
// its error and its error output.
func runPubkeyIn(ctx context.Context, stdin []byte, args ...string) (string, error) {
	c := PubkeyCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	var stderr bytes.Buffer
	c.SetIn(bytes.NewReader(stdin))
	c.SetOut(io.Discard)
	c.SetErr(&stderr)
	c.SetArgs(args)
	err := c.ExecuteContext(ctx)
	return stderr.String(), err
}

// keySession creates the user name with a new key and returns a context for a
// session that authenticated with that key, and the key.
func keySession(t *testing.T, ctx context.Context, be *backend.Backend, name string, admin bool) (context.Context, gossh.Signer) {
	t.Helper()
	signer := newEd25519Signer(t)
	pk := signer.PublicKey()
	u, err := be.CreateUser(ctx, name, proto.UserOptions{Admin: admin, PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(proto.WithUserContext(ctx, u), ssh.ContextKeyPublicKey, pk), signer
}

// proofFor is signer's proof for `pubkey add` by the account name.
func proofFor(t *testing.T, signer gossh.Signer, name string) []byte {
	t.Helper()
	return sshsigSign(t, signer, pubkeyProofNamespace, "sha512", "", []byte(name))
}

// Tracker 26: `pubkey add` took any public key, so anyone could claim a
// published key and act for its holder once it was, for example, listed as
// an admin key. A non-admin now proves they hold the private half with an
// SSHSIG signature of their username.
func TestPubkeyAddNeedsProof(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	aliceCtx, _ := keySession(t, ctx, be, "alice", false)
	keysOf := func(name string) int {
		t.Helper()
		u, err := be.User(ctx, name)
		is.NoErr(err)
		return len(u.PublicKeys())
	}

	key := newEd25519Signer(t)
	authorized := sshutils.MarshalAuthorizedKey(key.PublicKey())
	other := newEd25519Signer(t)
	for _, tc := range []struct {
		name  string
		proof []byte
	}{
		{"none", nil},
		{"garbage", []byte("not a signature")},
		{"by another key", proofFor(t, other, "alice")},
		{"for another account", proofFor(t, key, "bob")},
		{"in another namespace", sshsigSign(t, key, "file", "sha512", "", []byte("alice"))},
		{"too long", bytes.Repeat([]byte("a"), maxPubkeyProofSize+1)},
	} {
		_, err := runPubkeyIn(aliceCtx, tc.proof, "add", authorized)
		if !errors.Is(err, errNoPubkeyProof) {
			t.Errorf("proof %s: err = %v, want %v", tc.name, err, errNoPubkeyProof)
		}
	}
	is.Equal(keysOf("alice"), 1) // nothing was added

	_, err := runPubkeyIn(aliceCtx, proofFor(t, key, "alice"), "add", authorized)
	is.NoErr(err)
	is.Equal(keysOf("alice"), 2)

	// echo ends the name with a newline; that is accepted too.
	key2 := newEd25519Signer(t)
	_, err = runPubkeyIn(aliceCtx, sshsigSign(t, key2, pubkeyProofNamespace, "sha256", "", []byte("alice\n")), "add",
		sshutils.MarshalAuthorizedKey(key2.PublicKey()))
	is.NoErr(err)
	is.Equal(keysOf("alice"), 3)

	// An admin attaches keys without a proof.
	adminCtx, _ := keySession(t, ctx, be, "boss", true)
	_, err = runPubkeyIn(adminCtx, nil, "add", sshutils.MarshalAuthorizedKey(newEd25519Signer(t).PublicKey()))
	is.NoErr(err)
	is.Equal(keysOf("boss"), 2)
}

// The error for a key that is already attached says neither whose it is
// nor whether it is the caller's own.
func TestPubkeyAddTakenKeyIsGeneric(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	aliceCtx, aliceKey := keySession(t, ctx, be, "alice", false)
	adminCtx, _ := keySession(t, ctx, be, "boss", true)

	key := newEd25519Signer(t)
	authorized := sshutils.MarshalAuthorizedKey(key.PublicKey())
	_, err := be.CreateUser(ctx, "victim", proto.UserOptions{PublicKeys: []gossh.PublicKey{key.PublicKey()}})
	is.NoErr(err)

	for _, sess := range []context.Context{aliceCtx, adminCtx} {
		name := proto.UserFromContext(sess).Username()
		stderr, err := runPubkeyIn(sess, proofFor(t, key, name), "add", authorized)
		is.True(errors.Is(err, errPubkeyTaken))
		if strings.Contains(err.Error(), "victim") || strings.Contains(stderr, "victim") {
			t.Errorf("%s: the error names the key's owner: %v", name, err)
		}
	}
	// The caller's own key gets the same answer.
	_, err = runPubkeyIn(aliceCtx, proofFor(t, aliceKey, "alice"), "add",
		sshutils.MarshalAuthorizedKey(aliceKey.PublicKey()))
	is.True(errors.Is(err, errPubkeyTaken))
}

// Tracker 326(e): a session whose key an admin removed may not add a key in
// its place. The session resolved its account when it authenticated, so
// only the check in the transaction that adds the key sees the removal.
func TestPubkeyAddAfterSessionKeyRemoved(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	aliceCtx, _ := keySession(t, ctx, be, "alice", false)
	adminCtx, _ := keySession(t, ctx, be, "boss", true)
	keep := newEd25519Signer(t)
	is.NoErr(be.AddPublicKey(ctx, "alice", keep.PublicKey()))

	// The admin removes the session's key, as after it was compromised.
	is.NoErr(be.RemovePublicKey(ctx, "alice", sshutils.PublicKeyFromContext(aliceCtx)))

	key := newEd25519Signer(t)
	for _, sess := range []context.Context{aliceCtx, proto.WithUserContext(adminCtx, proto.UserFromContext(aliceCtx))} {
		_, err := runPubkeyIn(sess, proofFor(t, key, "alice"), "add", sshutils.MarshalAuthorizedKey(key.PublicKey()))
		is.True(errors.Is(err, backend.ErrSessionKeyRemoved))
	}
	u, err := be.User(ctx, "alice")
	is.NoErr(err)
	is.Equal(len(u.PublicKeys()), 1) // only the key the admin kept
}
