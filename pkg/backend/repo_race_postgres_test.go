package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// pausingRenameStore runs renamed inside a rename's transaction, right after
// the row is renamed and so locked.
type pausingRenameStore struct {
	store.Store
	renamed func(newName string)
}

func (s *pausingRenameStore) SetRepoNameByName(ctx context.Context, h db.Handler, name string, newName string) error {
	if err := s.Store.SetRepoNameByName(ctx, h, name, newName); err != nil {
		return err
	}
	s.renamed(newName)
	return nil
}

// raceRename starts renaming e's repository "repo" to "moved" and returns
// once the rename holds the row's lock, inside its transaction. Calling the
// returned function lets the rename commit and waits for it.
func raceRename(t *testing.T, e whEnv) (commit func()) {
	t.Helper()
	moved := make(chan struct{})
	release := make(chan struct{})
	e.be.store = &pausingRenameStore{Store: e.be.store, renamed: func(name string) {
		if name == "moved" {
			close(moved)
			<-release
		}
	}}
	done := make(chan error, 1)
	go func() { done <- e.be.RenameRepository(e.ctx, "repo", "moved") }()
	<-moved
	return func() {
		t.Helper()
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("RenameRepository() => %v", err)
		}
	}
}

// waitForLockWaiter waits until a backend of the test database waits for a
// row lock. It fails the test if op returns first, since op was to wait.
func waitForLockWaiter(t *testing.T, e whEnv, op <-chan error, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case err := <-op:
			t.Fatalf("%s returned without waiting for the rename: %v", name, err)
		default:
		}
		var waiting bool
		if err := e.be.db.GetContext(e.ctx, &waiting, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock');`); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not wait for the rename's row lock", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On Postgres a delete that waited for a rename of the same repository to
// commit finds no repository under the old name. It must say so, and leave
// the renamed repository's webhooks and LFS objects alone. It used to read
// the row without a lock, delete its webhooks, delete nothing and report
// success, then remove the LFS directory of the repository's id.
func TestDeleteRepositoryRacingRenamePostgres(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	e.createHook(t, whURL, webhook.EventPush)
	object := filepath.Join(e.cfg.DataPath, "lfs", strconv.FormatInt(e.repo.ID(), 10), "objects", "o")
	is.NoErr(os.MkdirAll(filepath.Dir(object), 0o755))
	is.NoErr(os.WriteFile(object, []byte("x"), 0o600))

	commit := raceRename(t, e)
	deleted := make(chan error, 1)
	go func() { deleted <- e.be.DeleteRepository(e.ctx, "repo") }()
	waitForLockWaiter(t, e, deleted, "DeleteRepository")
	commit()

	deleteErr := <-deleted
	r, err := e.be.Repository(e.ctx, "moved")
	is.NoErr(err)
	hooks, err := e.be.ListWebhooks(e.ctx, r)
	is.NoErr(err)
	is.Equal(len(hooks), 1) // the renamed repository's webhook is kept
	_, err = os.Stat(object)
	is.NoErr(err)                                        // and its LFS object
	is.True(errors.Is(deleteErr, proto.ErrRepoNotFound)) // the delete reported a miss
}

// On Postgres a setter that waited for a rename of the same repository to
// commit finds no repository under the old name and must say so. They used
// to look the repository up without a lock, update no row and report
// success: "repo private" then left the renamed repository public.
func TestRepoSettersRacingRenamePostgres(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(e whEnv) error{
		"SetPrivate":     func(e whEnv) error { return e.be.SetPrivate(e.ctx, "repo", true) },
		"SetHidden":      func(e whEnv) error { return e.be.SetHidden(e.ctx, "repo", true) },
		"SetDescription": func(e whEnv) error { return e.be.SetDescription(e.ctx, "repo", "d") },
		"SetProjectName": func(e whEnv) error { return e.be.SetProjectName(e.ctx, "repo", "p") },
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			e := newPostgresWebhookEnv(t)

			commit := raceRename(t, e)
			done := make(chan error, 1)
			go func() { done <- set(e) }()
			waitForLockWaiter(t, e, done, name)
			commit()

			err := <-done
			is.True(errors.Is(err, proto.ErrRepoNotFound))
			r, err := e.be.Repository(e.ctx, "moved")
			is.NoErr(err)
			is.True(!r.IsPrivate() && !r.IsHidden())
			is.Equal(r.Description(), "")
			is.Equal(r.ProjectName(), "")
		})
	}
}
