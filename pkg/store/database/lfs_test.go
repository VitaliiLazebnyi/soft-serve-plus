package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/matryer/is"
)

// listLFSLocks pages through every lock of repo 7, limit at a time, by the
// ID of the last lock of each page, calling between after each page but the
// last. It returns how many times each path was listed.
func listLFSLocks(t *testing.T, ctx context.Context, dbx *db.DB, limit int, between func(page int)) map[string]int {
	t.Helper()
	s := &Store{}
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
// are each listed exactly once. updated_at has one-second resolution, so
// pages ordered by it alone would bring ties back in no fixed order, and
// Postgres would list some locks twice and others never.
func TestLFSLocksPagingSameSecond(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	const n = 60
	for i := range n {
		if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "f"+strconv.Itoa(i), "refs/heads/main"); err != nil {
			t.Fatal(err)
		}
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
	s := &Store{}

	const n = 20
	for i := range n {
		if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "f"+strconv.Itoa(i), "refs/heads/main"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.GetLFSLockForPath(ctx, dbx, 7, "f0")
	is.NoErr(err)

	seen := listLFSLocks(t, ctx, dbx, 5, func(page int) {
		switch page {
		case 1:
			is.NoErr(s.DeleteLFSLock(ctx, dbx, 7, first.ID))
		case 2:
			if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "new", "refs/heads/main"); err != nil {
				t.Fatal(err)
			}
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

// A non-positive limit returns no locks on every driver. SQLite would
// otherwise treat a negative LIMIT as no limit at all.
func TestGetLFSLocksNonPositiveLimit(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "a.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "b.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{0, -1} {
		locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, limit)
		is.NoErr(err)
		is.Equal(len(locks), 0)
	}

	locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, 1)
	is.NoErr(err)
	is.Equal(len(locks), 1)
	locks, err = s.GetLFSLocks(ctx, dbx, 7, 0, 10)
	is.NoErr(err)
	is.Equal(len(locks), 2)
}

func TestLFSObjectStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const oid1 = "1111111111111111111111111111111111111111111111111111111111111111"
	const oid2 = "2222222222222222222222222222222222222222222222222222222222222222"

	// Sizes past 2 GiB must survive (migration 0007).
	const big = int64(5) << 30
	if err := s.CreateLFSObject(ctx, dbx, 7, oid1, big); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLFSObject(ctx, dbx, 7, oid2, 2); err != nil {
		t.Fatal(err)
	}
	// The same object may be stored for another repository, but only once
	// per repository.
	if err := s.CreateLFSObject(ctx, dbx, 8, oid1, big); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLFSObject(ctx, dbx, 7, oid1, big); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("CreateLFSObject(duplicate) error = %v, want ErrDuplicateKey", err)
	}

	o, err := s.GetLFSObjectByOid(ctx, dbx, 7, oid1)
	if err != nil {
		t.Fatal(err)
	}
	if o.Oid != oid1 || o.Size != big || o.RepoID != 7 {
		t.Errorf("GetLFSObjectByOid() = %+v", o)
	}
	if err := s.UpdateLFSObjectSize(ctx, dbx, 7, oid2, 3); err != nil {
		t.Fatal(err)
	}
	if o, err := s.GetLFSObjectByOid(ctx, dbx, 7, oid2); err != nil || o.Size != 3 {
		t.Errorf("size after UpdateLFSObjectSize = %d, %v, want 3", o.Size, err)
	}
	if os, err := s.GetLFSObjects(ctx, dbx, 7); err != nil || len(os) != 2 {
		t.Errorf("GetLFSObjects(7) = %d, %v, want 2", len(os), err)
	}
	if os, err := s.GetLFSObjects(ctx, dbx, 8); err != nil || len(os) != 1 || os[0].Oid != oid1 {
		t.Errorf("GetLFSObjects(8) = %+v, %v, want [oid1]", os, err)
	}
}

func TestLFSLockStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	if err := s.CreateUser(ctx, dbx, "other", false, nil); err != nil {
		t.Fatal(err)
	}
	other, err := s.FindUserByUsername(ctx, dbx, "other")
	if err != nil {
		t.Fatal(err)
	}

	// Paths are stored as given; see TestLFSLockPathStoredAsGiven.
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, owner, "a.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, other.ID, "b.bin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 8, owner, "a.bin", ""); err != nil {
		t.Fatal(err)
	}
	// One lock per path and repository.
	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, other.ID, "a.bin", ""); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("CreateLFSLockForUser(locked path) error = %v, want ErrDuplicateKey", err)
	}

	l, err := s.GetLFSLockForPath(ctx, dbx, 7, "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "a.bin" || l.UserID != owner || l.RepoID != 7 || l.Refname.String != "refs/heads/main" {
		t.Errorf("GetLFSLockForPath() = %+v", l)
	}
	if got, err := s.GetLFSLockByID(ctx, dbx, 7, l.ID); err != nil || got.Path != "a.bin" {
		t.Errorf("GetLFSLockByID() = %+v, %v", got, err)
	}
	if _, err := s.GetLFSLockByID(ctx, dbx, 8, l.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockByID(other repo) error = %v, want ErrRecordNotFound", err)
	}

	// Pages follow the lock ID: afterID is the last ID of the previous page.
	ls, err := s.GetLFSLocks(ctx, dbx, 7, 0, 1)
	if err != nil || len(ls) != 1 {
		t.Fatalf("GetLFSLocks(after 0, limit 1) = %d, %v, want 1", len(ls), err)
	}
	ls2, err := s.GetLFSLocks(ctx, dbx, 7, ls[0].ID, 1)
	if err != nil || len(ls2) != 1 || ls2[0].ID <= ls[0].ID {
		t.Fatalf("GetLFSLocks(after first, limit 1) = %+v, %v, want the other lock", ls2, err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, ls2[0].ID, 1); err != nil || len(ls) != 0 {
		t.Errorf("GetLFSLocks(after last, limit 1) = %d, %v, want 0", len(ls), err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10); err != nil || len(ls) != 2 {
		t.Errorf("GetLFSLocks(after 0, limit 10) = %d, %v, want 2", len(ls), err)
	}

	if err := s.DeleteLFSLock(ctx, dbx, 7, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLFSLockByID(ctx, dbx, 7, l.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockByID() after delete: error = %v, want ErrRecordNotFound", err)
	}
	b, err := s.GetLFSLockForPath(ctx, dbx, 7, "b.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLFSLock(ctx, dbx, 7, b.ID); err != nil {
		t.Fatal(err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10); err != nil || len(ls) != 0 {
		t.Errorf("GetLFSLocks(7) after deleting both = %d, %v, want 0", len(ls), err)
	}
}

// A lock is stored for the path it was asked for. Validation
// (lfs.ValidateLockPath) accepts a path that ends in a space, and the store
// does not trim it: "a.bin " does not lock "a.bin", and a lock on "a.bin"
// does not then fail as a duplicate.
func TestLFSLockPathStoredAsGiven(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const repo, owner = 7, 100 // see newWebhookTestDB

	for _, p := range []string{"a.bin ", "a.bin", " a.bin"} {
		if _, err := s.CreateLFSLockForUser(ctx, dbx, repo, owner, p, ""); err != nil {
			t.Fatalf("CreateLFSLockForUser(%q) => %v", p, err)
		}
		l, err := s.GetLFSLockForPath(ctx, dbx, repo, p)
		if err != nil || l.Path != p {
			t.Errorf("GetLFSLockForPath(%q) = %q, %v", p, l.Path, err)
		}
	}
}

// GetLFSObjectsByOids finds the repository's objects among the oids, in
// batches past the chunk size, and leaves out missing oids and another
// repository's objects.
func TestGetLFSObjectsByOids(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	oid := func(i int) string {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		return hex.EncodeToString(sum[:])
	}
	const n = lfsObjectsByOidsChunk + 10
	var want, oids []string
	for i := range n {
		if i%2 == 0 {
			if err := s.CreateLFSObject(ctx, dbx, 7, oid(i), int64(i)); err != nil {
				t.Fatal(err)
			}
			want = append(want, oid(i))
		}
		oids = append(oids, oid(i))
	}
	// Another repository's object with an oid asked for.
	if err := s.CreateLFSObject(ctx, dbx, 8, oid(1), 1); err != nil {
		t.Fatal(err)
	}

	objs, err := s.GetLFSObjectsByOids(ctx, dbx, 7, oids)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		if o.RepoID != 7 {
			t.Errorf("object %s of repository %d", o.Oid, o.RepoID)
		}
		got = append(got, o.Oid)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("found %d objects, want %d", len(got), len(want))
	}

	if objs, err := s.GetLFSObjectsByOids(ctx, dbx, 7, nil); err != nil || len(objs) != 0 {
		t.Errorf("GetLFSObjectsByOids(no oids) = %v, %v", objs, err)
	}
}

// Creates return the row they insert, and the lock reads name the owner.
func TestCreatesReturnTheirRow(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB

	r, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "/r.git", UserID: owner, Description: "d", Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.Name != "r" || r.Description != "d" || !r.Private || r.UserID != owner || r.CreatedAt.IsZero() {
		t.Errorf("CreateRepo() = %+v", r)
	}

	l, err := s.CreateLFSLockForUser(ctx, dbx, 7, owner, "a.bin", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if l.ID == 0 || l.Path != "a.bin" || l.UserID != owner || l.RepoID != 7 || l.Refname.String != "refs/heads/main" || l.CreatedAt.IsZero() {
		t.Errorf("CreateLFSLockForUser() = %+v", l)
	}
	for name, get := range map[string]func() (models.LFSLock, error){
		"GetLFSLockByID":    func() (models.LFSLock, error) { return s.GetLFSLockByID(ctx, dbx, 7, l.ID) },
		"GetLFSLockForPath": func() (models.LFSLock, error) { return s.GetLFSLockForPath(ctx, dbx, 7, "a.bin") },
		"GetLFSLocks": func() (models.LFSLock, error) {
			ls, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10)
			if err != nil || len(ls) != 1 {
				return models.LFSLock{}, err
			}
			return ls[0], nil
		},
	} {
		got, err := get()
		if err != nil || got.ID != l.ID || got.OwnerName != "owner" {
			t.Errorf("%s() = %+v, %v; want lock %d owned by owner", name, got, err, l.ID)
		}
	}

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	tok, err := s.CreateAccessToken(ctx, dbx, "t", owner, "hash", expires)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ID == 0 || tok.Name != "t" || tok.UserID != owner || tok.Token != "hash" || !tok.ExpiresAt.Valid || !tok.ExpiresAt.Time.Equal(expires) {
		t.Errorf("CreateAccessToken() = %+v", tok)
	}
}
