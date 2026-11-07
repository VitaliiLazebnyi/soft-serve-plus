package database

import (
	"context"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/matryer/is"
)

// listLFSLocks pages through every lock of repo 7, limit at a time, by the
// ID of the last lock of each page, calling between after each page but the
// last. It returns how many times each path was listed.
func listLFSLocks(t *testing.T, ctx context.Context, dbx *db.DB, limit int, between func(page int)) map[string]int {
	t.Helper()
	s := &lfsStore{}
	seen := map[string]int{}
	var after int64
	for page := 1; ; page++ {
		locks, err := s.GetLFSLocks(ctx, dbx, 7, after, limit)
		if err != nil {
			t.Fatalf("GetLFSLocks(after %d): %v", after, err)
		}
		for _, l := range locks {
			seen[l.Path]++
		}
		if len(locks) < limit {
			return seen
		}
		after = locks[len(locks)-1].ID
		if between != nil {
			between(page)
		}
	}
}

// TestLFSLocksPagingSameSecond checks that locks created in the same second
// are each listed exactly once. Pages used to be ordered by updated_at alone,
// which has one-second resolution, so ties came back in no fixed order and
// Postgres listed some locks twice and others never.
func TestLFSLocksPagingSameSecond(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &lfsStore{}

	const n = 60
	for i := range n {
		is.NoErr(s.CreateLFSLockForUser(ctx, dbx, 7, 100, "f"+strconv.Itoa(i), "refs/heads/main"))
	}
	_, err := dbx.ExecContext(ctx, `UPDATE lfs_locks SET updated_at = '2026-01-01 00:00:00';`)
	is.NoErr(err)

	seen := listLFSLocks(t, ctx, dbx, 7, nil)
	is.Equal(len(seen), n)
	for path, count := range seen {
		if count != 1 {
			t.Errorf("%s listed %d times, want 1", path, count)
		}
	}
}

// TestLFSLocksPagingConcurrentChanges checks that locks created or deleted
// between pages do not make the listing skip or repeat the other locks. With
// pages taken by row offset, a lock deleted from an earlier page shifted a
// later lock onto the page already returned, and a new lock shifted one onto
// the next page twice.
func TestLFSLocksPagingConcurrentChanges(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &lfsStore{}

	const n = 20
	for i := range n {
		is.NoErr(s.CreateLFSLockForUser(ctx, dbx, 7, 100, "f"+strconv.Itoa(i), "refs/heads/main"))
	}
	first, err := s.GetLFSLockForPath(ctx, dbx, 7, "f0")
	is.NoErr(err)

	seen := listLFSLocks(t, ctx, dbx, 5, func(page int) {
		switch page {
		case 1:
			is.NoErr(s.DeleteLFSLock(ctx, dbx, 7, first.ID))
		case 2:
			is.NoErr(s.CreateLFSLockForUser(ctx, dbx, 7, 100, "new", "refs/heads/main"))
		}
	})
	for i := range n {
		path := "f" + strconv.Itoa(i)
		if seen[path] != 1 {
			t.Errorf("%s listed %d times, want 1", path, seen[path])
		}
	}
	if seen["new"] > 1 {
		t.Errorf("new listed %d times, want at most 1", seen["new"])
	}
}
