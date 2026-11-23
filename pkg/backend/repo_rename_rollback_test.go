package backend

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
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
// so the row and the directory agree again.
func TestRenameRepositoryCommitFailureMovesDirectoryBack(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "team/old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}

	if be.RenameRepository(ctx, "team/old", "other/new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"team/old\", \"other/new\") != nil")
	}

	r, err := be.Repository(ctx, "team/old")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the old name
	_, err = os.Stat(filepath.Join(be.repoPath("team/old"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(be.repoPath("other/new"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
	_, err = os.Lstat(filepath.Join(cfg.DataPath, "repos", "other"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	} // no empty group left behind
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

// A COMMIT whose answer is lost, as when the connection drops during it,
// reports an error although the rename committed. The row then names the
// new name, so the directory stays there and the rename succeeds.
func TestRenameRepositoryCommitOutcomeLostKeepsDirectoryWithRow(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if err := be.RenameRepository(ctx, "old", "new"); err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, "new")
	if err != nil {
		t.Fatal(err)
	} // the row and the directory are under the new name
	_, err = r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(be.repoPath("old"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// failingByIDStore fails every lookup by ID, as a database that went away
// with the connection would.
type failingByIDStore struct {
	store.Store
}

func (failingByIDStore) GetRepoByIDForUpdate(context.Context, db.Handler, int64) (models.Repo, error) {
	return models.Repo{}, errors.New("driver: bad connection")
}

func (failingByIDStore) GetRepoByID(context.Context, db.Handler, int64) (models.Repo, error) {
	return models.Repo{}, errors.New("driver: bad connection")
}

// When the outcome cannot be read either, the directory stays where it is
// rather than risk moving it away from its row.
func TestRenameRepositoryCommitOutcomeUnknownLeavesDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	be.store = failingByIDStore{Store: be.store}
	be.renameTxDone = func(err error) error {
		if err == nil {
			return errors.New("driver: bad connection")
		}
		return err
	}

	if be.RenameRepository(ctx, "old", "new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"old\", \"new\") != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // where the committed row puts it
}

// After a failed rename, the directory is moved back only if the row still
// has the old name. A row renamed by another session since has a name this
// rename does not know of, so the directory stays where it is.
func TestRenameRepositoryFailureRenamedAgainLeavesDirectory(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	r, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}
	be.renameTxDone = func(err error) error {
		// Another session renames the row once this one's transaction
		// has failed.
		_, xerr := be.db.ExecContext(ctx, be.db.Rebind(`UPDATE repos SET name = 'third' WHERE id = ?`), r.ID())
		if xerr != nil {
			t.Fatal(xerr)
		}
		return err
	}

	if be.RenameRepository(ctx, "old", "new") == nil {
		t.Fatal("not true: be.RenameRepository(ctx, \"old\", \"new\") != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("new"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // left where it is
	_, err = os.Lstat(be.repoPath("old"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// On SQLite the re-check after a failed rename does not wait for the
// database's write lock: another writer holding it does not keep the
// directory from going back to its row, as it would once busy_timeout
// passed.
func TestRenameRepositoryRecheckSQLiteIgnoresWriteLock(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	if be.db.DriverName() != "sqlite" {
		t.Skip("SQLite only")
	}
	ctx := asAdmin(t, be, backendTestContext(be, cfg))
	_, err := be.CreateRepository(ctx, "old", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.db.ExecContext(ctx, `CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	be.store = unpinnableRenameStore{Store: be.store}

	// Another writer takes the write lock once the rename's transaction
	// has failed, and keeps it until the rename returns.
	var holder *sql.Tx
	be.renameTxDone = func(err error) error {
		var berr error
		holder, berr = beginRawTx(t, ctx, be.db)
		if berr != nil {
			t.Fatal(berr)
		}
		_, berr = holder.ExecContext(ctx, `CREATE TABLE held (n INTEGER)`)
		if berr != nil {
			t.Fatal(berr)
		}
		return err
	}
	renameErr := be.RenameRepository(ctx, "old", "new")
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if renameErr == nil {
		t.Fatal("not true: renameErr != nil")
	}
	_, err = os.Stat(filepath.Join(be.repoPath("old"), "HEAD"))
	if err != nil {
		t.Fatal(err)
	} // moved back to its row
	_, err = os.Lstat(be.repoPath("new"))
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}
