package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// unpinnableRenameStore renames the row and then records a row that breaks
// a deferred foreign key, so the rename's statements succeed and its commit
// fails, as a lost connection or a full disk would make it.
type unpinnableRenameStore struct {
	store.Store
}

func (s unpinnableRenameStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	if err := s.Store.SetRepoNameByName(ctx, h, name, newName); err != nil {
		return err
	}
	_, err := h.ExecContext(ctx, `INSERT INTO pin (repo_id) VALUES (999999)`)
	return err
}

// A rename whose transaction fails after the directory moved moves it back,
// so the row and the directory agree again. The directory used to stay at
// the new name, without a row, while the row still named the old one.
func TestRenameRepositoryCommitFailureMovesDirectoryBack(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "team/old", defaultAdmin(t, be), proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	is.NoErr(err)
	be.store = unpinnableRenameStore{Store: be.store}

	is.True(be.RenameRepository(ctx, "team/old", "other/new") != nil)

	r, err := be.Repository(ctx, "team/old")
	is.NoErr(err) // the row and the directory are under the old name
	_, err = os.Stat(filepath.Join(be.repoPath("team/old"), "HEAD"))
	is.NoErr(err)
	_, err = os.Lstat(be.repoPath("other/new"))
	is.True(os.IsNotExist(err))
	_, err = os.Lstat(filepath.Join(cfg.DataPath, "repos", "other"))
	is.True(os.IsNotExist(err)) // no empty group left behind
	_, err = r.Open()
	is.NoErr(err)
}
