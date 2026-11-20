package database

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/matryer/is"
)

// GetRepoWithCollabByName returns the repository with the caller's
// collaborator level, ok only when the caller has a row, and not found for
// a missing name.
func TestGetRepoWithCollabByName(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	is.NoErr(s.CreateUser(ctx, dbx, "alice", false, nil))
	alice, err := s.FindUserByUsername(ctx, dbx, "alice")
	is.NoErr(err)
	is.NoErr(s.AddCollabByUsernameAndRepo(ctx, dbx, "alice", "repo7", access.ReadWriteAccess))

	m, level, ok, err := s.GetRepoWithCollabByName(ctx, dbx, "repo7", alice.ID)
	is.NoErr(err)
	is.Equal(m.ID, int64(7))
	is.Equal(m.Name, "repo7")
	is.True(ok)
	is.Equal(level, access.ReadWriteAccess)

	m, _, ok, err = s.GetRepoWithCollabByName(ctx, dbx, "repo8", alice.ID)
	is.NoErr(err)
	is.Equal(m.ID, int64(8))
	is.True(!ok)

	_, _, ok, err = s.GetRepoWithCollabByName(ctx, dbx, "repo7", 0)
	is.NoErr(err)
	is.True(!ok)

	_, _, _, err = s.GetRepoWithCollabByName(ctx, dbx, "missing", alice.ID)
	is.True(errors.Is(err, db.ErrRecordNotFound))
}
