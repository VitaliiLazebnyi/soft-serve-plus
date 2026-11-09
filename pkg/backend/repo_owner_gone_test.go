package backend

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A repository whose owner was deleted meanwhile fails on the owner's
// foreign key, which is not the repository already existing.
func TestCreateRepositoryOwnerGone(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.DeleteUser(ctx, "alice"))

	_, err = be.CreateSystemRepository(ctx, "orphan", user, proto.RepositoryOptions{})
	is.True(err != nil)
	is.True(!errors.Is(err, proto.ErrRepoExist))
	is.True(errors.Is(err, db.ErrForeignKey))
	_, err = be.Repository(ctx, "orphan")
	is.True(err != nil)
}
