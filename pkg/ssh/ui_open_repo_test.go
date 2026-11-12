package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
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
	open := func(ui *UI, rn string) (proto.Repository, error) {
		msg, ok := ui.openRepoCmd(rn)().(repoOpenedMsg)
		if !ok {
			t.Fatal("openRepoCmd did not return a repoOpenedMsg")
		}
		return msg.repo, msg.err
	}

	is := is.New(t)
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	adminKey := uiTestKey(t)
	cfg.InitialAdminKeys = []string{sshutils.MarshalAuthorizedKey(adminKey)}

	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { _ = dbx.Close() })
	is.NoErr(migrate.Migrate(ctx, dbx))
	dbstore := database.New(ctx, dbx)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)

	pk := uiTestKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)

	// The session authenticated as owner; the key then stops resolving
	// to the account, so only the session user grants access.
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), owner)
	is.NoErr(be.RemovePublicKey(ctx, "owner", pk))
	if r, _ := be.ReadableByPublicKey(sessCtx, "private", pk); r != nil {
		t.Fatal("a by-key lookup still finds the owner; the test proves nothing")
	}

	ui := NewUI(common.NewCommon(sessCtx, 80, 40), "")
	r, err := open(ui, "private")
	is.NoErr(err)
	is.Equal(r.Name(), "private")

	// An admin key no account owns opens the private repository too.
	adminCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, adminKey)
	ui = NewUI(common.NewCommon(adminCtx, 80, 40), "")
	r, err = open(ui, "private")
	is.NoErr(err)
	is.Equal(r.Name(), "private")

	// Anyone else gets the missing-repository error.
	otherCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, uiTestKey(t))
	ui = NewUI(common.NewCommon(otherCtx, 80, 40), "")
	_, err = open(ui, "private")
	is.Equal(err, common.ErrMissingRepo)
}
