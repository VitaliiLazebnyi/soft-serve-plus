package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"golang.org/x/crypto/ssh"
)

// txCheckStore fails the test when one of the reads below runs inside a
// transaction.
type txCheckStore struct {
	store.Store
	t *testing.T
}

func (s txCheckStore) check(name string, h db.Handler) {
	s.t.Helper()
	if _, ok := h.(*db.Tx); ok {
		s.t.Errorf("%s ran in a transaction", name)
	}
}

func (s txCheckStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.AccessLevel, error) {
	s.check("GetAnonAccess", h)
	return s.Store.GetAnonAccess(ctx, h)
}

func (s txCheckStore) GetAllowKeylessAccess(ctx context.Context, h db.Handler) (bool, error) {
	s.check("GetAllowKeylessAccess", h)
	return s.Store.GetAllowKeylessAccess(ctx, h)
}

func (s txCheckStore) GetCollabByUsernameAndRepo(ctx context.Context, h db.Handler, username, repo string) (models.Collab, error) {
	s.check("GetCollabByUsernameAndRepo", h)
	return s.Store.GetCollabByUsernameAndRepo(ctx, h, username, repo)
}

func (s txCheckStore) ListCollabsByUserID(ctx context.Context, h db.Handler, userID int64) ([]models.Collab, error) {
	s.check("ListCollabsByUserID", h)
	return s.Store.ListCollabsByUserID(ctx, h, userID)
}

func (s txCheckStore) GetAllUsers(ctx context.Context, h db.Handler) ([]models.User, error) {
	s.check("GetAllUsers", h)
	return s.Store.GetAllUsers(ctx, h)
}

func (s txCheckStore) ListPublicKeysByUsername(ctx context.Context, h db.Handler, username string) ([]ssh.PublicKey, error) {
	s.check("ListPublicKeysByUsername", h)
	return s.Store.ListPublicKeysByUsername(ctx, h, username)
}

func (s txCheckStore) GetAllRepos(ctx context.Context, h db.Handler) ([]models.Repo, error) {
	s.check("GetAllRepos", h)
	return s.Store.GetAllRepos(ctx, h)
}

func (s txCheckStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.check("GetRepoByName", h)
	return s.Store.GetRepoByName(ctx, h, name)
}

// A read of one statement runs on the pool, not in a transaction: a read
// transaction takes three round trips to Postgres instead of one, and
// AnonAccess, AllowKeyless, Repository and IsCollaborator are on the access
// check of every request.
func TestSingleReadsRunWithoutTransaction(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "r", admin, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	be.store = txCheckStore{Store: be.store, t: t}

	be.AnonAccess(ctx)
	be.AllowKeyless(ctx)
	if _, _, err := be.IsCollaborator(ctx, "r", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Users(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.ListPublicKeys(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Repositories(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Repository(ctx, "r"); err != nil {
		t.Fatal(err)
	}
}
