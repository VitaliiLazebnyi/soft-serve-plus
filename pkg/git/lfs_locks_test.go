package git

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// TestLFSLockRangeRejectsInvalidCursor checks that listing locks over SSH
// refuses a cursor that is not a lock ID it hands out. A cursor that did not
// parse used to read as the first page. The backend has no database on
// purpose: a refused cursor must not reach it.
func TestLFSLockRangeRejectsInvalidCursor(t *testing.T) {
	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{ctx: context.Background(), op: lfs.OperationUpload},
		user:        lfsTestUser{},
	}

	for _, cursor := range []string{
		"abc",
		"1x",
		"0",
		"-1",
		"99999999999999999999",
	} {
		t.Run(cursor, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Range(%q) reached the database: %v", cursor, r)
				}
			}()
			_, err := l.Range(cursor, 10, func(transfer.Lock) error { return nil })
			if !errors.Is(err, transfer.ErrParseError) {
				t.Errorf("Range(%q) error = %v, want %v", cursor, err, transfer.ErrParseError)
			}
		})
	}
}

// pagedLockStore holds locks with the IDs 1 to n and returns them as the
// database store does: up to limit of them after afterID, in ID order.
type pagedLockStore struct {
	store.Store
	n int64
}

func (s pagedLockStore) GetLFSLocks(_ context.Context, _ db.Handler, repoID int64, afterID int64, limit int) ([]models.LFSLock, error) {
	var locks []models.LFSLock
	for id := afterID + 1; id <= s.n && len(locks) < limit; id++ {
		locks = append(locks, models.LFSLock{ID: id, RepoID: repoID, UserID: 1, Path: strconv.FormatInt(id, 10)})
	}
	return locks, nil
}

func (pagedLockStore) GetUserByID(_ context.Context, _ db.Handler, id int64) (models.User, error) {
	return models.User{ID: id, Username: "owner"}, nil
}

// TestLFSLockRangeCursor checks that listing locks over SSH hands out the ID
// of the last lock of a full page as the next cursor, and starts the next
// page after it, so paging lists every lock exactly once.
func TestLFSLockRangeCursor(t *testing.T) {
	dbx, err := db.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	l := &lfsLockBackend{
		lfsTransfer: &lfsTransfer{
			ctx:    context.Background(),
			dbx:    dbx,
			store:  pagedLockStore{n: 7},
			logger: log.New(io.Discard),
			repo:   lfsTestRepo{},
			op:     lfs.OperationUpload,
		},
		user: lfsTestUser{},
	}

	var ids, cursors []string
	cursor := ""
	for range 10 {
		next, err := l.Range(cursor, 3, func(lock transfer.Lock) error {
			ids = append(ids, lock.ID())
			return nil
		})
		if err != nil {
			t.Fatalf("Range(%q): %v", cursor, err)
		}
		if next == "" {
			break
		}
		cursors = append(cursors, next)
		cursor = next
	}
	if got, want := strings.Join(ids, ","), "1,2,3,4,5,6,7"; got != want {
		t.Errorf("listed locks %s, want %s", got, want)
	}
	if got, want := strings.Join(cursors, ","), "3,6"; got != want {
		t.Errorf("next cursors %s, want %s", got, want)
	}
}
