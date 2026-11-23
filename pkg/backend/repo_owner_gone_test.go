package backend

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A repository whose owner was deleted meanwhile fails on the owner's
// foreign key, which is not the repository already existing.
func TestCreateRepositoryOwnerGone(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateSystemRepository(ctx, "orphan", user, proto.RepositoryOptions{})
	if !(err != nil) {
		t.Fatal("not true: err != nil")
	}
	if errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: !errors.Is(err, proto.ErrRepoExist)")
	}
	if !errors.Is(err, db.ErrForeignKey) {
		t.Fatal("not true: errors.Is(err, db.ErrForeignKey)")
	}
	_, err = be.Repository(ctx, "orphan")
	if !(err != nil) {
		t.Fatal("not true: err != nil")
	}
}
