package selection

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	gossh "golang.org/x/crypto/ssh"
)

// The list shows the repositories of the session's user, as the session
// resolved it. A lookup of the session's key that fails no longer turns the
// list into what an anonymous caller sees.
func TestSelectionListsSessionUserRepos(t *testing.T) {
	ctx, be := newTestBackend(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk := signer.PublicKey()
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "mine", owner, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatal(err)
	}

	// Key lookups now fail; the session's user was resolved before.
	if _, err := db.FromContext(ctx).ExecContext(ctx, "ALTER TABLE public_keys RENAME TO public_keys_gone"); err != nil {
		t.Fatal(err)
	}

	sessCtx := context.WithValue(proto.WithUserContext(ctx, owner), ssh.ContextKeyPublicKey, pk)
	c := common.NewCommon(sessCtx, 80, 40)
	t.Cleanup(c.Zone.Close)
	s := New(c)
	s.SetSize(80, 40)
	if !runLoad(t, s, s.Init()) {
		t.Fatal("init did not load the repositories")
	}
	if got := listedNames(s); !slices.Contains(got, "mine") {
		t.Fatalf("listed %v, want the session user's private repository", got)
	}
}
