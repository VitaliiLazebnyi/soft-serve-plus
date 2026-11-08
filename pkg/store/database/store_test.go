package database

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"golang.org/x/crypto/ssh"
)

// newStoreTestDB opens a migrated database (see newWebhookTestDB) and a store
// on it.
func newStoreTestDB(t *testing.T) (context.Context, *db.DB, store.Store) {
	t.Helper()
	ctx, dbx := newWebhookTestDB(t)
	return ctx, dbx, New(ctx, dbx)
}

func newPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func sameKeys(a, b []ssh.PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sshutils.KeysEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestUserStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	k1, k2, k3 := newPublicKey(t), newPublicKey(t), newPublicKey(t)

	// Usernames are stored lower-cased and looked up case-insensitively.
	if err := s.CreateUser(ctx, dbx, "Alice", false, []ssh.PublicKey{k1, k2}); err != nil {
		t.Fatal(err)
	}
	u, err := s.FindUserByUsername(ctx, dbx, "ALICE")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.Admin {
		t.Errorf("FindUserByUsername() = %+v, want non-admin alice", u)
	}
	if got, err := s.GetUserByID(ctx, dbx, u.ID); err != nil || got.Username != "alice" {
		t.Errorf("GetUserByID(%d) = %+v, %v, want alice", u.ID, got, err)
	}
	if got, err := s.FindUserByPublicKey(ctx, dbx, k2); err != nil || got.ID != u.ID {
		t.Errorf("FindUserByPublicKey(k2) = %+v, %v, want alice", got, err)
	}
	if _, err := s.FindUserByPublicKey(ctx, dbx, k3); !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
		t.Errorf("FindUserByPublicKey(unknown key) error = %v, want ErrRecordNotFound", err)
	}

	// Keys are listed in the order they were added.
	if err := s.AddPublicKeyByUsername(ctx, dbx, "alice", k3); err != nil {
		t.Fatal(err)
	}
	pks, err := s.ListPublicKeysByUsername(ctx, dbx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !sameKeys(pks, []ssh.PublicKey{k1, k2, k3}) {
		t.Errorf("ListPublicKeysByUsername() = %d keys, want k1, k2, k3 in order", len(pks))
	}
	if err := s.RemovePublicKeyByUsername(ctx, dbx, "alice", k2); err != nil {
		t.Fatal(err)
	}
	pks, err = s.ListPublicKeysByUserID(ctx, dbx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameKeys(pks, []ssh.PublicKey{k1, k3}) {
		t.Errorf("ListPublicKeysByUserID() after removing k2 = %d keys, want k1, k3", len(pks))
	}

	// A key belongs to one account only.
	if err := s.CreateUser(ctx, dbx, "bob", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPublicKeyByUsername(ctx, dbx, "bob", k1); !errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
		t.Errorf("AddPublicKeyByUsername(bob, alice's key) error = %v, want ErrDuplicateKey", err)
	}
	if err := s.CreateUser(ctx, dbx, "BOB", false, nil); !errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
		t.Errorf("CreateUser(BOB) with bob taken: error = %v, want ErrDuplicateKey", err)
	}
	if err := s.CreateUser(ctx, dbx, "-bad name", false, nil); err == nil {
		t.Error("CreateUser(invalid username) = nil error, want a validation error")
	}

	if err := s.SetAdminByUsername(ctx, dbx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUsernameByUsername(ctx, dbx, "alice", "Carol"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserPasswordByUsername(ctx, dbx, "carol", "hash"); err != nil {
		t.Fatal(err)
	}
	u, err = s.FindUserByUsername(ctx, dbx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin || !u.Password.Valid || u.Password.String != "hash" {
		t.Errorf("carol = %+v, want an admin with password %q", u, "hash")
	}
	if _, err := s.FindUserByUsername(ctx, dbx, "alice"); !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
		t.Errorf("FindUserByUsername(alice) after rename: error = %v, want ErrRecordNotFound", err)
	}

	users, err := s.GetAllUsers(ctx, dbx)
	if err != nil {
		t.Fatal(err)
	}
	// admin (seeded), owner (newWebhookTestDB), carol, bob.
	if len(users) != 4 {
		t.Errorf("GetAllUsers() = %d users, want 4", len(users))
	}

	// Deleting a user deletes their keys with them.
	if err := s.DeleteUserByUsername(ctx, dbx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindUserByPublicKey(ctx, dbx, k1); !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
		t.Errorf("FindUserByPublicKey(k1) after deleting its user: error = %v, want ErrRecordNotFound", err)
	}
}

func TestRepoStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB

	// Names are sanitised: no leading slash, no .git suffix.
	if err := s.CreateRepo(ctx, dbx, "/team/app.git", owner, "App", "an app", true, true, false, false); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepoByName(ctx, dbx, "team/app")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "team/app" || r.ProjectName != "App" || r.Description != "an app" ||
		!r.Private || !r.Hidden || r.Mirror || r.Anonymous || !r.UserID.Valid || r.UserID.Int64 != owner {
		t.Errorf("GetRepoByName(team/app) = %+v", r)
	}
	if got, err := s.GetRepoByID(ctx, dbx, r.ID); err != nil || got.Name != "team/app" {
		t.Errorf("GetRepoByID(%d) = %+v, %v", r.ID, got, err)
	}
	if err := s.CreateRepo(ctx, dbx, "team/app", owner, "", "", false, false, false, false); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("CreateRepo(existing name) error = %v, want ErrDuplicateKey", err)
	}
	if _, err := s.GetRepoByName(ctx, dbx, "nope"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetRepoByName(nope) error = %v, want ErrRecordNotFound", err)
	}

	err = dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := s.SetRepoDescriptionByName(ctx, tx, "team/app.git", "new description"); err != nil {
			return err
		}
		if err := s.SetRepoProjectNameByName(ctx, tx, "team/app", "New App"); err != nil {
			return err
		}
		if err := s.SetRepoIsPrivateByName(ctx, tx, "team/app", false); err != nil {
			return err
		}
		if err := s.SetRepoIsHiddenByName(ctx, tx, "team/app", false); err != nil {
			return err
		}
		if err := s.SetRepoIsMirrorByName(ctx, tx, "team/app", true); err != nil {
			return err
		}
		if _, err := s.GetRepoByNameForUpdate(ctx, tx, "team/app"); err != nil {
			return err
		}
		return s.LockUserRepos(ctx, tx, owner)
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetRepoDescriptionByName(ctx, dbx, "team/app"); err != nil || v != "new description" {
		t.Errorf("GetRepoDescriptionByName() = %q, %v", v, err)
	}
	if v, err := s.GetRepoProjectNameByName(ctx, dbx, "team/app"); err != nil || v != "New App" {
		t.Errorf("GetRepoProjectNameByName() = %q, %v", v, err)
	}
	if v, err := s.GetRepoIsPrivateByName(ctx, dbx, "team/app"); err != nil || v {
		t.Errorf("GetRepoIsPrivateByName() = %v, %v, want false", v, err)
	}
	if v, err := s.GetRepoIsHiddenByName(ctx, dbx, "team/app"); err != nil || v {
		t.Errorf("GetRepoIsHiddenByName() = %v, %v, want false", v, err)
	}
	if v, err := s.GetRepoIsMirrorByName(ctx, dbx, "team/app"); err != nil || !v {
		t.Errorf("GetRepoIsMirrorByName() = %v, %v, want true", v, err)
	}

	// Anonymous repositories are owned by someone but don't count against
	// their limit.
	if err := s.CreateRepo(ctx, dbx, "anon", owner, "", "", false, false, false, true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountUserRepos(ctx, dbx, owner); err != nil || n != 3 { // repo7, repo8, team/app
		t.Errorf("CountUserRepos() = %d, %v, want 3", n, err)
	}
	if n, err := s.CountAnonymousRepos(ctx, dbx); err != nil || n != 1 {
		t.Errorf("CountAnonymousRepos() = %d, %v, want 1", n, err)
	}
	if rs, err := s.GetUserRepos(ctx, dbx, owner); err != nil || len(rs) != 4 {
		t.Errorf("GetUserRepos() = %d repos, %v, want 4", len(rs), err)
	}

	if err := s.SetRepoNameByName(ctx, dbx, "team/app", "/team/renamed.git"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetRepoByID(ctx, dbx, r.ID); err != nil || got.Name != "team/renamed" {
		t.Errorf("repo after SetRepoNameByName = %+v, %v, want team/renamed", got, err)
	}

	if err := s.DeleteRepoByName(ctx, dbx, "team/renamed"); err != nil {
		t.Fatal(err)
	}
	if rs, err := s.GetAllRepos(ctx, dbx); err != nil || len(rs) != 3 {
		t.Errorf("GetAllRepos() after delete = %d repos, %v, want 3", len(rs), err)
	}
}

func TestCollabStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	if err := s.CreateUser(ctx, dbx, "dev", false, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.AddCollabByUsernameAndRepo(ctx, dbx, "DEV", "/repo7.git", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7")
	if err != nil {
		t.Fatal(err)
	}
	if c.RepoID != 7 || c.AccessLevel != access.ReadWriteAccess {
		t.Errorf("GetCollabByUsernameAndRepo() = %+v, want read-write on repo 7", c)
	}
	if err := s.AddCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7", access.ReadOnlyAccess); !errors.Is(db.WrapError(err), db.ErrDuplicateKey) {
		t.Errorf("AddCollabByUsernameAndRepo(again) error = %v, want ErrDuplicateKey", err)
	}

	cs, err := s.ListCollabsByRepo(ctx, dbx, "repo7")
	if err != nil || len(cs) != 1 {
		t.Errorf("ListCollabsByRepo(repo7) = %d, %v, want 1", len(cs), err)
	}
	us, err := s.ListCollabsByRepoAsUsers(ctx, dbx, "repo7")
	if err != nil || len(us) != 1 || us[0].Username != "dev" {
		t.Errorf("ListCollabsByRepoAsUsers(repo7) = %+v, %v, want [dev]", us, err)
	}
	if cs, err := s.ListCollabsByRepo(ctx, dbx, "repo8"); err != nil || len(cs) != 0 {
		t.Errorf("ListCollabsByRepo(repo8) = %d, %v, want 0", len(cs), err)
	}

	if err := s.RemoveCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCollabByUsernameAndRepo(ctx, dbx, "dev", "repo7"); !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
		t.Errorf("GetCollabByUsernameAndRepo() after remove: error = %v, want ErrRecordNotFound", err)
	}
}

func TestAccessTokenStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	if err := s.CreateUser(ctx, dbx, "other", false, nil); err != nil {
		t.Fatal(err)
	}
	other, err := s.FindUserByUsername(ctx, dbx, "other")
	if err != nil {
		t.Fatal(err)
	}

	forever, err := s.CreateAccessToken(ctx, dbx, "forever", owner, "hash-forever", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if forever.Name != "forever" || forever.UserID != owner || forever.Token != "hash-forever" || forever.ExpiresAt.Valid {
		t.Errorf("CreateAccessToken(no expiry) = %+v", forever)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	expiring, err := s.CreateAccessToken(ctx, dbx, "expiring", owner, "hash-expiring", expires)
	if err != nil {
		t.Fatal(err)
	}
	if !expiring.ExpiresAt.Valid || !expiring.ExpiresAt.Time.Equal(expires) {
		t.Errorf("CreateAccessToken(expiring) expires at %v, want %v", expiring.ExpiresAt, expires)
	}

	if got, err := s.GetAccessTokenByToken(ctx, dbx, "hash-expiring"); err != nil || got.ID != expiring.ID {
		t.Errorf("GetAccessTokenByToken() = %+v, %v, want token %d", got, err, expiring.ID)
	}
	if got, err := s.GetAccessTokenByToken(ctx, dbx, "hash-forever"); err != nil || got.UserID != owner {
		t.Errorf("GetAccessTokenByToken() = %+v, %v, want user %d", got, err, owner)
	}
	if ts, err := s.GetAccessTokensByUserID(ctx, dbx, owner); err != nil || len(ts) != 2 {
		t.Errorf("GetAccessTokensByUserID() = %d, %v, want 2", len(ts), err)
	}

	// A user cannot delete someone else's token.
	if err := s.DeleteAccessTokenForUser(ctx, dbx, other.ID, forever.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccessToken(ctx, dbx, forever.ID); err != nil {
		t.Errorf("token deleted by another user: GetAccessToken() error = %v", err)
	}
	if err := s.DeleteAccessTokenForUser(ctx, dbx, owner, forever.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccessToken(ctx, dbx, forever.ID); !errors.Is(db.WrapError(err), db.ErrRecordNotFound) {
		t.Errorf("GetAccessToken() after its owner deleted it: error = %v, want ErrRecordNotFound", err)
	}
	if err := s.DeleteAccessToken(ctx, dbx, expiring.ID); err != nil {
		t.Fatal(err)
	}
	if ts, err := s.GetAccessTokensByUserID(ctx, dbx, owner); err != nil || len(ts) != 0 {
		t.Errorf("GetAccessTokensByUserID() after deleting both = %d, %v, want 0", len(ts), err)
	}
}

func TestSettingsStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)

	// The defaults the first migration seeds.
	if v, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || !v {
		t.Errorf("GetAllowKeylessAccess() = %v, %v, want true", v, err)
	}
	if v, err := s.GetAnonAccess(ctx, dbx); err != nil || v != access.ReadOnlyAccess {
		t.Errorf("GetAnonAccess() = %v, %v, want read-only", v, err)
	}

	for _, allow := range []bool{false, true, false} {
		if err := s.SetAllowKeylessAccess(ctx, dbx, allow); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || v != allow {
			t.Errorf("GetAllowKeylessAccess() after setting %v = %v, %v", allow, v, err)
		}
	}
	for _, level := range []access.AccessLevel{access.NoAccess, access.ReadWriteAccess, access.AdminAccess, access.ReadOnlyAccess} {
		if err := s.SetAnonAccess(ctx, dbx, level); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetAnonAccess(ctx, dbx); err != nil || v != level {
			t.Errorf("GetAnonAccess() after setting %v = %v, %v", level, v, err)
		}
	}
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
	if os, err := s.GetLFSObjectsByName(ctx, dbx, "repo8"); err != nil || len(os) != 1 || os[0].Oid != oid1 {
		t.Errorf("GetLFSObjectsByName(repo8) = %+v, %v, want [oid1]", os, err)
	}

	if err := s.DeleteLFSObjectByOid(ctx, dbx, 7, oid1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLFSObjectByOid(ctx, dbx, 7, oid1); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSObjectByOid() after delete: error = %v, want ErrRecordNotFound", err)
	}
	if _, err := s.GetLFSObjectByOid(ctx, dbx, 8, oid1); err != nil {
		t.Errorf("deleting repo 7's copy removed repo 8's: %v", err)
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

	// Paths are stored without surrounding space or a leading slash.
	if err := s.CreateLFSLockForUser(ctx, dbx, 7, owner, " /a.bin ", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLFSLockForUser(ctx, dbx, 7, other.ID, "b.bin", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLFSLockForUser(ctx, dbx, 8, owner, "a.bin", ""); err != nil {
		t.Fatal(err)
	}
	// One lock per path and repository.
	if err := s.CreateLFSLockForUser(ctx, dbx, 7, other.ID, "a.bin", ""); !errors.Is(err, db.ErrDuplicateKey) {
		t.Errorf("CreateLFSLockForUser(locked path) error = %v, want ErrDuplicateKey", err)
	}

	l, err := s.GetLFSLockForPath(ctx, dbx, 7, "/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "a.bin" || l.UserID != owner || l.RepoID != 7 || l.Refname != "refs/heads/main" {
		t.Errorf("GetLFSLockForPath() = %+v", l)
	}
	if got, err := s.GetLFSLockByID(ctx, dbx, 7, l.ID); err != nil || got.Path != "a.bin" {
		t.Errorf("GetLFSLockByID() = %+v, %v", got, err)
	}
	if _, err := s.GetLFSLockByID(ctx, dbx, 8, l.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockByID(other repo) error = %v, want ErrRecordNotFound", err)
	}
	if got, err := s.GetLFSLockForUserPath(ctx, dbx, 7, owner, "a.bin"); err != nil || got.ID != l.ID {
		t.Errorf("GetLFSLockForUserPath() = %+v, %v", got, err)
	}
	if _, err := s.GetLFSLockForUserPath(ctx, dbx, 7, other.ID, "a.bin"); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockForUserPath(not the owner) error = %v, want ErrRecordNotFound", err)
	}
	if _, err := s.GetLFSLockForUserByID(ctx, dbx, 7, other.ID, l.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockForUserByID(not the owner) error = %v, want ErrRecordNotFound", err)
	}
	if ls, err := s.GetLFSLocksForUser(ctx, dbx, 7, owner); err != nil || len(ls) != 1 {
		t.Errorf("GetLFSLocksForUser() = %d, %v, want 1", len(ls), err)
	}

	// Pages: the count is of every lock in the repository.
	ls, n, err := s.GetLFSLocksWithCount(ctx, dbx, 7, 1, 1)
	if err != nil || len(ls) != 1 || n != 2 {
		t.Errorf("GetLFSLocksWithCount(page 1, limit 1) = %d locks, count %d, %v, want 1 and 2", len(ls), n, err)
	}
	ls2, err := s.GetLFSLocks(ctx, dbx, 7, 2, 1)
	if err != nil || len(ls2) != 1 || (len(ls) == 1 && ls2[0].ID == ls[0].ID) {
		t.Errorf("GetLFSLocks(page 2, limit 1) = %+v, %v, want the other lock", ls2, err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, 3, 1); err != nil || len(ls) != 0 {
		t.Errorf("GetLFSLocks(page 3, limit 1) = %d, %v, want 0", len(ls), err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10); err != nil || len(ls) != 2 {
		t.Errorf("GetLFSLocks(page 0) = %d, %v, want page 1 with 2 locks", len(ls), err)
	}

	// Only the owner's delete removes it; DeleteLFSLock (force) removes any.
	if err := s.DeleteLFSLockForUserByID(ctx, dbx, 7, other.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLFSLockByID(ctx, dbx, 7, l.ID); err != nil {
		t.Errorf("lock deleted by another user: %v", err)
	}
	if err := s.DeleteLFSLockForUserByID(ctx, dbx, 7, owner, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLFSLockByID(ctx, dbx, 7, l.ID); !errors.Is(err, db.ErrRecordNotFound) {
		t.Errorf("GetLFSLockByID() after owner delete: error = %v, want ErrRecordNotFound", err)
	}
	b, err := s.GetLFSLockForPath(ctx, dbx, 7, "b.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLFSLock(ctx, dbx, 7, b.ID); err != nil {
		t.Fatal(err)
	}
	if ls, err := s.GetLFSLocks(ctx, dbx, 7, 1, 10); err != nil || len(ls) != 0 {
		t.Errorf("GetLFSLocks(7) after deleting both = %d, %v, want 0", len(ls), err)
	}
}
