package git

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// lfsIDRepo and lfsIDUser carry the ID of a row in the test database.
type lfsIDRepo struct {
	proto.Repository
	id int64
}

func (r lfsIDRepo) ID() int64  { return r.id }
func (lfsIDRepo) Name() string { return "repo" }

type lfsIDUser struct {
	proto.User
	id int64
}

func (u lfsIDUser) ID() int64 { return u.id }

// TestLFSLockLookupsDoNotWaitForWriter checks that reading LFS locks does not
// take the SQLite write lock: the lookups must succeed while another
// connection holds a write transaction open, instead of waiting for it
// (busy_timeout) and then failing with "database is locked".
func TestLFSLockLookupsDoNotWaitForWriter(t *testing.T) {
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, "sqlite", filepath.Join(dp, "test.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := database.New(ctx, dbx)

	var repoID, userID int64
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		u, err := s.FindUserByUsername(ctx, tx, "admin")
		if err != nil {
			return err
		}
		userID = u.ID
		if err := s.CreateRepo(ctx, tx, "repo", userID, "", "", false, false, false, false); err != nil {
			return err
		}
		r, err := s.GetRepoByName(ctx, tx, "repo")
		if err != nil {
			return err
		}
		repoID = r.ID
		return s.CreateLFSLockForUser(ctx, tx, repoID, userID, "foo.png", "refs/heads/main")
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	newBackend := func(ctx context.Context) *lfsLockBackend {
		return &lfsLockBackend{
			lfsTransfer: &lfsTransfer{
				ctx:    ctx,
				dbx:    dbx,
				store:  s,
				logger: log.New(io.Discard),
				repo:   lfsIDRepo{id: repoID},
				op:     lfs.OperationUpload,
			},
			user: lfsIDUser{id: userID},
		}
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- dbx.TransactionContext(ctx, func(tx *db.Tx) error {
			if err := s.CreateUser(ctx, tx, "writer", false, nil); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	func() {
		defer close(release)
		// Each lookup gets its own deadline, well under busy_timeout, so a
		// lookup that waits for the writer fails instead of slowing the test
		// down, and does not use up the time of the lookups after it.
		var cancels []context.CancelFunc
		defer func() {
			for _, cancel := range cancels {
				cancel()
			}
		}()
		backend := func() *lfsLockBackend {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			cancels = append(cancels, cancel)
			return newBackend(c)
		}

		lock, err := backend().FromPath("foo.png")
		if err != nil {
			t.Errorf("FromPath: %v", err)
		} else if _, err := backend().FromID(lock.ID()); err != nil {
			t.Errorf("FromID: %v", err)
		}
		var n int
		if _, err := backend().Range("", 10, func(transfer.Lock) error { n++; return nil }); err != nil || n != 1 {
			t.Errorf("Range = %d locks, %v; want 1, nil", n, err)
		}
	}()

	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
}
