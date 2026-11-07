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
	_, err = be.ImportRepository(userCtx, AboutRepo, user, offlineRemote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	is.NoErr(err)
	is.True(errors.Is(be.RenameRepository(userCtx, "mine", AboutRepo), proto.ErrUnauthorized))

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
