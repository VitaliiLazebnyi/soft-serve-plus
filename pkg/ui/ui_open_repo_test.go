package ui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"reflect"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
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
	open := func(ui *Model, rn string) (proto.Repository, error) {
		msg, ok := ui.openRepoCmd(rn)().(repoOpenedMsg)
		if !ok {
			t.Fatal("openRepoCmd did not return a repoOpenedMsg")
		}
		return msg.repo, msg.err
	}

	adminKey := uiTestKey(t)
	e := backendtest.New(t, backendtest.WithConfig(func(cfg *config.Config) {
		cfg.InitialAdminKeys = []string{proto.MarshalAuthorizedKey(adminKey)}
	}))
	ctx, cfg, be := t.Context(), e.Cfg, e.Backend

	pk := uiTestKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	// The session authenticated as owner; the key then stops resolving
	// to the account, so only the session user grants access.
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), owner)
	if err := be.RemovePublicKey(ctx, "owner", pk); err != nil {
		t.Fatal(err)
	}
	if _, err := be.UserByPublicKey(ctx, pk); !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatalf("the key still resolves (%v); the test proves nothing", err)
	}

	ui := New(uitest.NewCommon(t, sessCtx, cfg, be), 80, 40, "")
	r, err := open(ui, "private")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Name(); got != "private" {
		t.Fatalf("r.Name() = %v, want %v", got, "private")
	}

	// An admin key no account owns opens the private repository too.
	adminCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, adminKey)
	ui = New(uitest.NewCommon(t, adminCtx, cfg, be), 80, 40, "")
	r, err = open(ui, "private")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Name(); got != "private" {
		t.Fatalf("r.Name() = %v, want %v", got, "private")
	}

	// Anyone else gets the missing-repository error.
	otherCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, uiTestKey(t))
	ui = New(uitest.NewCommon(t, otherCtx, cfg, be), 80, 40, "")
	_, err = open(ui, "private")
	if got, want := err, common.ErrMissingRepo; !reflect.DeepEqual(got, want) {
		t.Fatalf("err = %v, want %v", got, want)
	}
}
