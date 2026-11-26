package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// runPubkeyIn runs a `pubkey` subcommand with stdin as its input and returns
// its error and its error output.
func runPubkeyIn(ctx context.Context, stdin []byte, args ...string) (string, error) {
	c := cmdsOf(ctx).PubkeyCommand()
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
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	aliceCtx, _ := keySession(t, ctx, be, "alice", false)
	keysOf := func(name string) int {
		t.Helper()
		u, err := be.User(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return len(u.PublicKeys())
	}

	key := newEd25519Signer(t)
	authorized := proto.MarshalAuthorizedKey(key.PublicKey())
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
	if got := keysOf("alice"); got != 1 {
		t.Fatalf("keysOf(\"alice\") = %v, want %v", got, 1)
	} // nothing was added

	_, err := runPubkeyIn(aliceCtx, proofFor(t, key, "alice"), "add", authorized)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf("alice"); got != 2 {
		t.Fatalf("keysOf(\"alice\") = %v, want %v", got, 2)
	}

	// echo ends the name with a newline; that is accepted too.
	key2 := newEd25519Signer(t)
	_, err = runPubkeyIn(aliceCtx, sshsigSign(t, key2, pubkeyProofNamespace, "sha256", "", []byte("alice\n")), "add",
		proto.MarshalAuthorizedKey(key2.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf("alice"); got != 3 {
		t.Fatalf("keysOf(\"alice\") = %v, want %v", got, 3)
	}

	// An admin attaches keys without a proof.
	adminCtx, _ := keySession(t, ctx, be, "boss", true)
	_, err = runPubkeyIn(adminCtx, nil, "add", proto.MarshalAuthorizedKey(newEd25519Signer(t).PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf("boss"); got != 2 {
		t.Fatalf("keysOf(\"boss\") = %v, want %v", got, 2)
	}
}

// The error for a key that is already attached says neither whose it is
// nor whether it is the caller's own.
func TestPubkeyAddTakenKeyIsGeneric(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	aliceCtx, aliceKey := keySession(t, ctx, be, "alice", false)
	adminCtx, _ := keySession(t, ctx, be, "boss", true)

	key := newEd25519Signer(t)
	authorized := proto.MarshalAuthorizedKey(key.PublicKey())
	_, err := be.CreateUser(ctx, "victim", proto.UserOptions{PublicKeys: []gossh.PublicKey{key.PublicKey()}})
	if err != nil {
		t.Fatal(err)
	}

	for _, sess := range []context.Context{aliceCtx, adminCtx} {
		name := proto.UserFromContext(sess).Username()
		stderr, err := runPubkeyIn(sess, proofFor(t, key, name), "add", authorized)
		if !errors.Is(err, errPubkeyTaken) {
			t.Fatal("not true: errors.Is(err, errPubkeyTaken)")
		}
		if strings.Contains(err.Error(), "victim") || strings.Contains(stderr, "victim") {
			t.Errorf("%s: the error names the key's owner: %v", name, err)
		}
	}
	// The caller's own key gets the same answer.
	_, err = runPubkeyIn(aliceCtx, proofFor(t, aliceKey, "alice"), "add",
		proto.MarshalAuthorizedKey(aliceKey.PublicKey()))
	if !errors.Is(err, errPubkeyTaken) {
		t.Fatal("not true: errors.Is(err, errPubkeyTaken)")
	}
}

// Tracker 326(e): a session whose key an admin removed may not add a key in
// its place. The session resolved its account when it authenticated, so
// only the check in the transaction that adds the key sees the removal.
func TestPubkeyAddAfterSessionKeyRemoved(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	aliceCtx, _ := keySession(t, ctx, be, "alice", false)
	adminCtx, _ := keySession(t, ctx, be, "boss", true)
	keep := newEd25519Signer(t)
	if err := be.AddPublicKey(ctx, "alice", keep.PublicKey()); err != nil {
		t.Fatal(err)
	}

	// The admin removes the session's key, as after it was compromised.
	if err := be.RemovePublicKey(ctx, "alice", proto.PublicKeyFromContext(aliceCtx)); err != nil {
		t.Fatal(err)
	}

	key := newEd25519Signer(t)
	for _, sess := range []context.Context{aliceCtx, proto.WithUserContext(adminCtx, proto.UserFromContext(aliceCtx))} {
		_, err := runPubkeyIn(sess, proofFor(t, key, "alice"), "add", proto.MarshalAuthorizedKey(key.PublicKey()))
		if !errors.Is(err, backend.ErrSessionKeyRemoved) {
			t.Fatal("not true: errors.Is(err, backend.ErrSessionKeyRemoved)")
		}
	}
	u, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(u.PublicKeys()); got != 1 {
		t.Fatalf("len(u.PublicKeys()) = %v, want %v", got, 1)
	} // only the key the admin kept
}

func newTestPubkey(t *testing.T) gossh.PublicKey {
	t.Helper()
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	return kp.PublicKey()
}

func runPubkey(ctx context.Context, args ...string) error {
	c := cmdsOf(ctx).PubkeyCommand()
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SetArgs(args)
	return c.ExecuteContext(ctx)
}

// A user may not remove the key their session authenticated with, and in
// particular not their only key; removing another of their keys still works.
func TestPubkeyRemoveRefusesSessionAndLastKey(t *testing.T) {
	t.Parallel()
	ctx, be := newAuthTestContext(t)
	k1, k2 := newTestPubkey(t), newTestPubkey(t)
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{k1}}); err != nil {
		t.Fatal(err)
	}
	ctx1 := context.WithValue(ctx, ssh.ContextKeyPublicKey, k1)
	ctx2 := context.WithValue(ctx, ssh.ContextKeyPublicKey, k2)

	keyCount := func() int {
		t.Helper()
		u, err := be.User(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		return len(u.PublicKeys())
	}

	// Only key: refused as the last key.
	if err := runPubkey(ctx1, "remove", proto.MarshalAuthorizedKey(k1)); !errors.Is(err, errRemoveLastKey) {
		t.Fatalf("removing the only key: got %v, want errRemoveLastKey", err)
	}
	// A trailing comment does not change the key.
	if err := runPubkey(ctx1, "remove", proto.MarshalAuthorizedKey(k1), "laptop"); !errors.Is(err, errRemoveLastKey) {
		t.Fatalf("removing the only key with a comment: got %v, want errRemoveLastKey", err)
	}
	if n := keyCount(); n != 1 {
		t.Fatalf("got %d keys, want 1", n)
	}

	// With a second key, the session key is still refused.
	if err := be.AddPublicKey(ctx, "alice", k2); err != nil {
		t.Fatal(err)
	}
	if err := runPubkey(ctx1, "remove", proto.MarshalAuthorizedKey(k1)); !errors.Is(err, errRemoveSessionKey) {
		t.Fatalf("removing the session key: got %v, want errRemoveSessionKey", err)
	}
	if n := keyCount(); n != 2 {
		t.Fatalf("got %d keys, want 2", n)
	}

	// Connected with the other key, the old one can be removed (rotation).
	if err := runPubkey(ctx2, "remove", proto.MarshalAuthorizedKey(k1)); err != nil {
		t.Fatalf("removing another key: %v", err)
	}
	if n := keyCount(); n != 1 {
		t.Fatalf("got %d keys, want 1", n)
	}
	// And now k2 is the last one.
	if err := runPubkey(ctx2, "remove", proto.MarshalAuthorizedKey(k2)); !errors.Is(err, errRemoveLastKey) {
		t.Fatalf("removing the new only key: got %v, want errRemoveLastKey", err)
	}
}
