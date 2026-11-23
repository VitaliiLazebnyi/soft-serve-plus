package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/matryer/is"
)

// A repository needs an owner: repos.user_id is NOT NULL. CreateRepo says so
// instead of sending an insert that cannot succeed.
func TestCreateRepoRequiresOwner(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}

	_, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "ownerless", UserID: 0, Anonymous: true})
	is.True(err != nil)
	if _, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "owned", UserID: 100}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepoByName(ctx, dbx, "owned")
	is.NoErr(err)
	is.Equal(r.UserID, int64(100))
}

// Changing a repository or a user moves its updated_at.
func TestUpdatesSetUpdatedAt(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	old := "2001-01-01 00:00:00"
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE repos SET updated_at = ?;`), old)
	is.NoErr(err)
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE users SET updated_at = ?;`), old)
	is.NoErr(err)
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
		is.NoErr(err)
		is.NoErr(set("repo7"))
		r, err := rs.GetRepoByName(ctx, dbx, "repo7")
		is.NoErr(err)
		if !r.UpdatedAt.After(cutoff) {
			t.Errorf("setting the %s left updated_at at %v", name, r.UpdatedAt)
		}
	}
	is.NoErr(rs.SetRepoNameByName(ctx, dbx, "repo8", "renamed"))
	r, err := rs.GetRepoByName(ctx, dbx, "renamed")
	is.NoErr(err)
	is.True(r.UpdatedAt.After(cutoff))

	us := &Store{}
	user := func(name string) models.User {
		t.Helper()
		u, err := us.FindUserByUsername(ctx, dbx, name)
		is.NoErr(err)
		return u
	}
	is.NoErr(us.SetAdminByUsername(ctx, dbx, "owner", true))
	is.True(user("owner").UpdatedAt.After(cutoff))
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE users SET updated_at = ?;`), old)
	is.NoErr(err)
	is.NoErr(us.SetUsernameByUsername(ctx, dbx, "owner", "owner2"))
	is.True(user("owner2").UpdatedAt.After(cutoff))
}

// allow_keyless is stored as the text "true" or "false", as migration 1
// seeds it, on every driver.
func TestAllowKeylessStoredAsText(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	for _, allow := range []bool{false, true} {
		is.NoErr(s.SetAllowKeylessAccess(ctx, dbx, allow))
		var raw string
		is.NoErr(dbx.GetContext(ctx, &raw, `SELECT value FROM settings WHERE "key" = 'allow_keyless'`))
		want := "false"
		if allow {
			want = "true"
		}
		is.Equal(raw, want)
		got, err := s.GetAllowKeylessAccess(ctx, dbx)
		is.NoErr(err)
		is.Equal(got, allow)
	}
}

// An anon_access value that is no access level is an error, not a level
// below no-access that the access checks would have to know about.
func TestAnonAccessInvalidValue(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	_, err := dbx.ExecContext(ctx, dbx.Rebind(`UPDATE settings SET value = ? WHERE "key" = 'anon_access'`), "everyone")
	is.NoErr(err)
	_, err = s.GetAnonAccess(ctx, dbx)
	is.True(errors.Is(err, access.ErrInvalidAccessLevel))
}

// lfs_locks.refname is nullable; a lock without one can still be read.
func TestLFSLockWithoutRefname(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	_, err := dbx.ExecContext(ctx, `INSERT INTO lfs_locks (repo_id, user_id, path, refname, updated_at)
		VALUES (7, 100, 'a.bin', NULL, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	s := &Store{}
	locks, err := s.GetLFSLocks(ctx, dbx, 7, 0, 10)
	is.NoErr(err)
	is.Equal(len(locks), 1)
	is.True(!locks[0].Refname.Valid)

	if _, err := s.CreateLFSLockForUser(ctx, dbx, 7, 100, "b.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	lock, err := s.GetLFSLockForPath(ctx, dbx, 7, "b.bin")
	is.NoErr(err)
	is.Equal(lock.Refname.String, "refs/heads/main")
}

// A webhook subscribed to several of the requested events is returned once.
func TestGetWebhooksByRepoIDWhereEventReturnsEachOnce(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	id, err := s.CreateWebhook(ctx, dbx, models.Webhook{RepoID: 7, URL: "https://93.184.216.34/a", ContentType: 0, Active: true})
	is.NoErr(err)
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, id, []int{1, 2, 3}))

	whs, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1, 2, 3})
	is.NoErr(err)
	is.Equal(idsOf(whs), []int64{id})
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
