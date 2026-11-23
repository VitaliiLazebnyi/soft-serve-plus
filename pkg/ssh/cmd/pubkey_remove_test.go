package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

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
