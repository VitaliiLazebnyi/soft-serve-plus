package database

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

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
