package backend

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// AboutRepo is reserved for server admins whatever AdminOnlyCreate says: a
// registered user and an anonymous caller can neither create nor import it,
// nor rename a repository to it.
func TestAboutRepoReservedForAdmins(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, AboutRepo, user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(userCtx, "/"+AboutRepo+".git", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, AboutRepo, nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	// A case variant takes the same directory on a case-insensitive
	// filesystem, so it is reserved too.
	_, err = be.CreateRepository(userCtx, ".Soft-Serve", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, ".SOFT-SERVE", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, ".Soft-Serve", user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, AboutRepo, user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RenameRepository(userCtx, "mine", AboutRepo), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", AboutRepo), proto.ErrUnauthorized)")
	}
	if !errors.Is(be.RenameRepository(userCtx, "mine", ".Soft-Serve"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", \".Soft-Serve\"), proto.ErrUnauthorized)")
	}

	_, err = be.Repository(ctx, AboutRepo)
	if !(err != nil) {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
}

// A server admin may create AboutRepo, and rename a repository to it.
func TestAboutRepoAllowedForAdmins(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, AboutRepo, admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(adminCtx, AboutRepo); err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(adminCtx, "draft", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RenameRepository(adminCtx, "draft", AboutRepo); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, AboutRepo)
	if err != nil {
		t.Fatal(err)
	}
}

// AboutRepoOwnedByAdmin holds only for an AboutRepo an admin created and
// still owns. One an anonymous caller created, which is recorded with the
// default admin as owner, is refused: such a repository can predate the
// reservation of the name for admins.
func TestAboutRepoOwnedByAdmin(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	aboutRepo := func() proto.Repository {
		t.Helper()
		repos, err := be.Repositories(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range repos {
			if r.Name() == AboutRepo {
				return r
			}
		}
		t.Fatalf("%s not found", AboutRepo)
		return nil
	}

	// Created by an admin: shown, until the owner is no longer an admin.
	_, err = be.CreateRepository(adminCtx, AboutRepo, admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !be.AboutRepoOwnedByAdmin(ctx, aboutRepo()) {
		t.Fatal("not true: be.AboutRepoOwnedByAdmin(ctx, aboutRepo())")
	}
	if err := be.SetAdmin(ctx, "root", false); err != nil {
		t.Fatal(err)
	}
	if be.AboutRepoOwnedByAdmin(ctx, aboutRepo()) {
		t.Fatal("not true: !be.AboutRepoOwnedByAdmin(ctx, aboutRepo())")
	}
	if err := be.SetAdmin(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(adminCtx, AboutRepo); err != nil {
		t.Fatal(err)
	}

	// Created anonymously before the name was reserved: the row names an
	// admin as owner, but it is not shown.
	if _, err := be.store.CreateRepo(ctx, be.db, models.Repo{
		Name:      AboutRepo,
		UserID:    admin.ID(),
		Anonymous: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := aboutRepo()
	if got, want := r.UserID(), admin.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if be.AboutRepoOwnedByAdmin(ctx, r) {
		t.Fatal("not true: !be.AboutRepoOwnedByAdmin(ctx, r)")
	}
}
