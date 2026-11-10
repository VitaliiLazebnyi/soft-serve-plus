package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// The SSH authentication middleware resolves the session's key to a user
// once and stores it in the context. Commands that act on the caller's
// account use that user instead of looking the key up again: with the
// user on the context they work even when the database cannot be reached.
func TestAccountCommandsUseSessionUser(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), user)

	// Any lookup from here on fails.
	if err := db.FromContext(ctx).Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
		want string
	}{
		{"info", InfoCommand, nil, "Username: alice"},
		{"pubkey list", PubkeyCommand, []string{"list"}, strings.TrimSpace(string(kp.AuthorizedKey()))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd()
			var out bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			if err := c.ExecuteContext(sessCtx); err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output %q does not contain %q", out.String(), tc.want)
			}
		})
	}
}
