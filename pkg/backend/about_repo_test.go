package backend

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// AboutRepo is reserved for server admins whatever AdminOnlyCreate says: a
// registered user and an anonymous caller can neither create nor import it,
// nor rename a repository to it.
func TestAboutRepoReservedForAdmins(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	is.True(!cfg.AdminOnlyCreate)
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, AboutRepo, user, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.CreateRepository(userCtx, "/"+AboutRepo+".git", user, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.CreateRepository(ctx, AboutRepo, nil, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	// A case variant takes the same directory on a case-insensitive
	// filesystem, so it is reserved too.
	_, err = be.CreateRepository(userCtx, ".Soft-Serve", user, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.CreateRepository(ctx, ".SOFT-SERVE", nil, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.ImportRepository(userCtx, ".Soft-Serve", user, offlineRemote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.ImportRepository(userCtx, AboutRepo, user, offlineRemote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	is.NoErr(err)
	is.True(errors.Is(be.RenameRepository(userCtx, "mine", AboutRepo), proto.ErrUnauthorized))
	is.True(errors.Is(be.RenameRepository(userCtx, "mine", ".Soft-Serve"), proto.ErrUnauthorized))

	_, err = be.Repository(ctx, AboutRepo)
	is.True(err != nil)
	_, err = be.Repository(ctx, "mine")
	is.NoErr(err)
}

// A server admin may create AboutRepo, and rename a repository to it.
func TestAboutRepoAllowedForAdmins(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	is.NoErr(err)
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, AboutRepo, admin, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.DeleteRepository(adminCtx, AboutRepo))

	_, err = be.CreateRepository(adminCtx, "draft", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.RenameRepository(adminCtx, "draft", AboutRepo))
	_, err = be.Repository(ctx, AboutRepo)
	is.NoErr(err)
}

// AboutRepoOwnedByAdmin holds only for an AboutRepo an admin created and
// still owns. One an anonymous caller created, which is recorded with the
// default admin as owner, is refused: such a repository can predate the
// reservation of the name for admins.
func TestAboutRepoOwnedByAdmin(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	is.NoErr(err)
	adminCtx := proto.WithUserContext(ctx, admin)

	aboutRepo := func() proto.Repository {
		t.Helper()
		repos, err := be.Repositories(ctx)
		is.NoErr(err)
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
	is.NoErr(err)
	is.True(be.AboutRepoOwnedByAdmin(ctx, aboutRepo()))
	is.NoErr(be.SetAdmin(ctx, "root", false))
	is.True(!be.AboutRepoOwnedByAdmin(ctx, aboutRepo()))
	is.NoErr(be.SetAdmin(ctx, "root", true))
	is.NoErr(be.DeleteRepository(adminCtx, AboutRepo))

	// Created anonymously before the name was reserved: the row names an
	// admin as owner, but it is not shown.
	is.NoErr(be.store.CreateRepo(ctx, be.db, AboutRepo, admin.ID(), "", "", false, false, false, true))
	r := aboutRepo()
	is.Equal(r.UserID(), admin.ID())
	is.True(!be.AboutRepoOwnedByAdmin(ctx, r))
}
