package database

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"golang.org/x/crypto/ssh"
)

// newStoreTestDB opens a migrated database (see newWebhookTestDB) and a store
// on it.
func newStoreTestDB(t *testing.T) (context.Context, *db.DB, store.Store) {
	t.Helper()
	ctx, dbx := newWebhookTestDB(t)
	return ctx, dbx, New(nil)
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
		if !proto.KeysEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// A repository needs an owner: repos.user_id is NOT NULL. CreateRepo says so
// instead of sending an insert that cannot succeed.
func TestCreateRepoRequiresOwner(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	_, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "ownerless", UserID: 0, Anonymous: true})
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if _, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "owned", UserID: 100}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepoByName(ctx, dbx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.UserID, int64(100); got != want {
		t.Fatalf("r.UserID = %v, want %v", got, want)
	}
}

// Changing a repository or a user moves its updated_at.
func TestUpdatesSetUpdatedAt(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	old := "2001-01-01 00:00:00"
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE repos SET updated_at = ?;`), old)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE users SET updated_at = ?;`), old)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	rs := &Store{}
	for name, set := range map[string]func(name string) error{
		"description":  func(n string) error { return rs.SetRepoDescriptionByName(ctx, dbx, n, "d") },
		"private":      func(n string) error { return rs.SetRepoIsPrivateByName(ctx, dbx, n, true) },
		"hidden":       func(n string) error { return rs.SetRepoIsHiddenByName(ctx, dbx, n, true) },
		"mirror":       func(n string) error { return rs.SetRepoIsMirrorByName(ctx, dbx, n, true) },
		"project name": func(n string) error { return rs.SetRepoProjectNameByName(ctx, dbx, n, "p") },
	} {
		_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE repos SET updated_at = ? WHERE name = 'repo7';`), old)
		if err != nil {
			t.Fatal(err)
		}
		if err := set("repo7"); err != nil {
			t.Fatal(err)
		}
		r, err := rs.GetRepoByName(ctx, dbx, "repo7")
		if err != nil {
			t.Fatal(err)
		}
		if !r.UpdatedAt.After(cutoff) {
			t.Errorf("setting the %s left updated_at at %v", name, r.UpdatedAt)
		}
	}
	if err := rs.SetRepoNameByName(ctx, dbx, "repo8", "renamed"); err != nil {
		t.Fatal(err)
	}
	r, err := rs.GetRepoByName(ctx, dbx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt.After(cutoff) {
		t.Fatal("not true: r.UpdatedAt.After(cutoff)")
	}

	us := &Store{}
	user := func(name string) models.User {
		t.Helper()
		u, err := us.FindUserByUsername(ctx, dbx, name)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if err := us.SetAdminByUsername(ctx, dbx, "owner", true); err != nil {
		t.Fatal(err)
	}
	if !user("owner").UpdatedAt.After(cutoff) {
		t.Fatal("not true: user(\"owner\").UpdatedAt.After(cutoff)")
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE users SET updated_at = ?;`), old)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetUsernameByUsername(ctx, dbx, "owner", "owner2"); err != nil {
		t.Fatal(err)
	}
	if !user("owner2").UpdatedAt.After(cutoff) {
		t.Fatal("not true: user(\"owner2\").UpdatedAt.After(cutoff)")
	}
}

// allow_keyless is stored as the text "true" or "false", as migration 1
// seeds it, on every driver.
func TestAllowKeylessStoredAsText(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	for _, allow := range []bool{false, true} {
		if err := s.SetAllowKeylessAccess(ctx, dbx, allow); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := dbx.GetContext(ctx, &raw, `SELECT value FROM settings WHERE "key" = 'allow_keyless'`); err != nil {
			t.Fatal(err)
		}
		want := "false"
		if allow {
			want = "true"
		}
		if got := raw; got != want {
			t.Fatalf("raw = %v, want %v", got, want)
		}
		got, err := s.GetAllowKeylessAccess(ctx, dbx)
		if err != nil {
			t.Fatal(err)
		}
		if got := got; got != allow {
			t.Fatalf("got = %v, want %v", got, allow)
		}
	}
}

// An anon_access value that is no access level is an error, not a level
// below no-access that the access checks would have to know about.
func TestAnonAccessInvalidValue(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE settings SET value = ? WHERE "key" = 'anon_access'`), "everyone")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.GetAnonAccess(ctx, dbx)
	if !errors.Is(err, access.ErrInvalidAccessLevel) {
		t.Fatal("not true: errors.Is(err, access.ErrInvalidAccessLevel)")
	}
}

// lfs_locks.refname is nullable; a lock without one can still be read.
func TestLFSLockWithoutRefname(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	_, err := dbx.ExecContext(ctx, `INSERT INTO lfs_locks (repo_id, user_id, path, refname, updated_at)
		VALUES (7, 100, 'a.bin', NULL, CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{}
	locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(locks); got != 1 {
		t.Fatalf("len(locks) = %v, want %v", got, 1)
	}
	if locks[0].Refname.Valid {
		t.Fatal("not true: !locks[0].Refname.Valid")
	}

	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "b.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	lock, err := s.GetLFSLockForPath(ctx, dbx, 7, "b.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Refname.String; got != "refs/heads/main" {
		t.Fatalf("lock.Refname.String = %v, want %v", got, "refs/heads/main")
	}
}

// A webhook subscribed to several of the requested events is returned once.
func TestGetWebhooksByRepoIDWhereEventReturnsEachOnce(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWebhookEvents(ctx, dbx, id, []int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}

	whs, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idsOf(whs), ([]int64{id}); !reflect.DeepEqual(got, want) {
		t.Fatalf("idsOf(whs) = %v, want %v", got, want)
	}
}

// vanishOnInsert installs a trigger that deletes webhook id when a delivery
// for it is inserted, after the insert's existence check has passed.
func vanishOnInsert(t *testing.T, ctx context.Context, dbx *db.DB, id int64) {
	t.Helper()
	triggers := []string{fmt.Sprintf(`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
		WHEN NEW.webhook_id = %d
		BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; END;`, id)}
	if !isSqlite(dbx) {
		triggers = []string{
			`CREATE FUNCTION vanish() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; RETURN NEW; END $$;`,
			fmt.Sprintf(`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
			FOR EACH ROW WHEN (NEW.webhook_id = %d) EXECUTE FUNCTION vanish();`, id),
		}
	}
	for _, q := range triggers {
		if _, err := dbx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// A write that matches no row reports db.ErrRecordNotFound rather than
// success, so a caller that raced a rename or a delete does not tell the
// user the change was made.
func TestWritesToMissingRowsAreNotFound(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const repo = 7 // see newWebhookTestDB

	hook, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: repo, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"DeleteRepoByName":         s.DeleteRepoByName(ctx, dbx, "nope"),
		"SetRepoIsPrivateByName":   s.SetRepoIsPrivateByName(ctx, dbx, "nope", true),
		"SetRepoIsHiddenByName":    s.SetRepoIsHiddenByName(ctx, dbx, "nope", true),
		"SetRepoIsMirrorByName":    s.SetRepoIsMirrorByName(ctx, dbx, "nope", true),
		"SetRepoDescriptionByName": s.SetRepoDescriptionByName(ctx, dbx, "nope", "d"),
		"SetRepoProjectNameByName": s.SetRepoProjectNameByName(ctx, dbx, "nope", "p"),
		// Another repository's webhook is not this one's.
		"UpdateWebhookByID":        s.UpdateWebhookByID(ctx, dbx, models.Webhook{ID: hook, RepoID: repo + 1, URL: "https://93.184.216.34/b", ContentType: 0, Active: true}),
		"DeleteWebhookForRepoByID": s.DeleteWebhookForRepoByID(ctx, dbx, repo+1, hook),
	} {
		if !errors.Is(err, db.ErrRecordNotFound) {
			t.Errorf("%s on a missing row: err = %v, want %v", name, err, db.ErrRecordNotFound)
		}
	}
}

// The settings setters write a setting whose row was deleted, rather than
// failing on the missing row.
func TestSettingsSettersRestoreMissingRows(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	if _, err := dbx.ExecContext(ctx, `DELETE FROM settings`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnonAccess(ctx, dbx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowKeylessAccess(ctx, dbx, false); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetAnonAccess(ctx, dbx); err != nil || l != access.ReadWriteAccess {
		t.Errorf("GetAnonAccess() = %v, %v; want read-write", l, err)
	}
	if a, err := s.GetAllowKeylessAccess(ctx, dbx); err != nil || a {
		t.Errorf("GetAllowKeylessAccess() = %v, %v; want false", a, err)
	}
	// And again, over the row they wrote.
	if err := s.SetAnonAccess(ctx, dbx, access.NoAccess); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetAnonAccess(ctx, dbx); err != nil || l != access.NoAccess {
		t.Errorf("GetAnonAccess() = %v, %v; want no-access", l, err)
	}
}

// Lists shown to users come back in a stable order. Rows are inserted out of
// order and then updated, which on Postgres moves an updated row to the end
// of a plain table scan.
func TestUserVisibleListsAreOrdered(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	rs, us, cs, ts, ls := &Store{}, &Store{}, &Store{}, &Store{}, &Store{}

	for _, name := range []string{"c", "a", "b"} {
		if _, err := rs.CreateRepo(ctx, dbx, models.Repo{Name: name, UserID: 100, ProjectName: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rs.SetRepoDescriptionByName(ctx, dbx, "repo7", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := rs.SetRepoDescriptionByName(ctx, dbx, "a", "moved"); err != nil {
		t.Fatal(err)
	}

	repos, err := rs.GetAllRepos(ctx, dbx)
	if err != nil {
		t.Fatal(err)
	}
	userRepos, err := rs.GetUserRepos(ctx, dbx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{repoNames(repos), repoNames(userRepos)} {
		if got, want := list, ([]string{"a", "b", "c", "repo7", "repo8"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("list = %v, want %v", got, want)
		}
	}

	for _, name := range []string{"zed", "amy"} {
		if err := us.CreateUser(ctx, dbx, name, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := us.SetAdminByUsername(ctx, dbx, "amy", true); err != nil {
		t.Fatal(err)
	}
	users, err := us.GetAllUsers(ctx, dbx)
	if err != nil {
		t.Fatal(err)
	}
	var usernames []string
	for _, u := range users {
		usernames = append(usernames, u.Username)
	}
	if got, want := usernames, ([]string{"admin", "amy", "owner", "zed"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("usernames = %v, want %v", got, want)
	}

	for _, name := range []string{"zed", "owner", "amy"} {
		if err := cs.AddCollabByUsernameAndRepo(ctx, dbx, name, "repo7", access.ReadOnlyAccess); err != nil {
			t.Fatal(err)
		}
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE collabs SET access_level = ? WHERE user_id = 100;`), access.ReadWriteAccess)
	if err != nil {
		t.Fatal(err)
	}
	collabs, err := cs.ListCollabsByRepoID(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(collabs); got != 3 {
		t.Fatalf("len(collabs) = %v, want %v", got, 3)
	}
	for i := 1; i < len(collabs); i++ {
		if collabs[i-1].ID >= collabs[i].ID {
			t.Fatal("not true: collabs[i-1].ID < collabs[i].ID")
		}
	}
	collabUsers, err := cs.ListCollabsByRepoIDAsUsers(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	usernames = nil
	for _, u := range collabUsers {
		usernames = append(usernames, u.Username)
	}
	if got, want := usernames, ([]string{"amy", "owner", "zed"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("usernames = %v, want %v", got, want)
	}

	var tokenIDs []int64
	for i := range 3 {
		tok, err := ts.CreateAccessToken(ctx, dbx, fmt.Sprintf("t%d", i), 100, fmt.Sprintf("hash%d", i), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		tokenIDs = append(tokenIDs, tok.ID)
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE access_tokens SET name = ? WHERE id = ?;`), "renamed", tokenIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := ts.GetAccessTokensByUserID(ctx, dbx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var gotTokenIDs []int64
	for _, tok := range tokens {
		gotTokenIDs = append(gotTokenIDs, tok.ID)
	}
	if got, want := gotTokenIDs, tokenIDs; !reflect.DeepEqual(got, want) {
		t.Fatalf("gotTokenIDs = %v, want %v", got, want)
	}

	for _, oid := range []string{"oid1", "oid2", "oid3"} {
		if err := ls.CreateLFSObject(ctx, dbx, 7, oid, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := ls.UpdateLFSObjectSize(ctx, dbx, 7, "oid1", 2); err != nil {
		t.Fatal(err)
	}
	objs, err := ls.GetLFSObjects(ctx, dbx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lfsOids(objs), ([]string{"oid1", "oid2", "oid3"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("lfsOids(objs) = %v, want %v", got, want)
	}
}

func repoNames(repos []models.Repo) []string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return names
}

func lfsOids(objs []models.LFSObject) []string {
	oids := make([]string, len(objs))
	for i, o := range objs {
		oids[i] = o.Oid
	}
	return oids
}
