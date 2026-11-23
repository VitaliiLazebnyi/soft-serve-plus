package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
)

func uiTestKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// openRepo uses the user AuthenticationMiddleware stored on the session
// context instead of resolving the key again each time a repository is
// opened, and an admin key on the context still opens every repository.
func TestOpenRepoUsesSessionUser(t *testing.T) {
	t.Parallel()
	open := func(ui *UI, rn string) (proto.Repository, error) {
		msg, ok := ui.openRepoCmd(rn)().(repoOpenedMsg)
		if !ok {
			t.Fatal("openRepoCmd did not return a repoOpenedMsg")
		}
		return msg.repo, msg.err
	}

	is := is.New(t)
	cfg := newTestUIConfig(t)
	adminKey := uiTestKey(t)
	cfg.InitialAdminKeys = []string{proto.MarshalAuthorizedKey(adminKey)}
	is.NoErr(cfg.Validate())

	ctx := t.Context()
	be := newTestUIBackend(t, cfg)

	pk := uiTestKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	// The session authenticated as owner; the key then stops resolving
	// to the account, so only the session user grants access.
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), owner)
	is.NoErr(be.RemovePublicKey(ctx, "owner", pk))
	if _, err := be.UserByPublicKey(ctx, pk); !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatalf("the key still resolves (%v); the test proves nothing", err)
	}

	ui := NewUI(uitest.NewCommon(t, sessCtx, cfg, be), 80, 40, "")
	r, err := open(ui, "private")
	is.NoErr(err)
	is.Equal(r.Name(), "private")

	// An admin key no account owns opens the private repository too.
	adminCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, adminKey)
	ui = NewUI(uitest.NewCommon(t, adminCtx, cfg, be), 80, 40, "")
	r, err = open(ui, "private")
	is.NoErr(err)
	is.Equal(r.Name(), "private")

	// Anyone else gets the missing-repository error.
	otherCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, uiTestKey(t))
	ui = NewUI(uitest.NewCommon(t, otherCtx, cfg, be), 80, 40, "")
	_, err = open(ui, "private")
	is.Equal(err, common.ErrMissingRepo)
}
