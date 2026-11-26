package database

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
)

// A failed sync records its error and keeps the time of the last one that
// succeeded; a sync that succeeds records its time and clears the error.
// Neither counts as an update of the repository.
func TestSetRepoMirrorSyncByID(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	m, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "m", UserID: owner, Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	get := func() (time.Time, bool, string, time.Time) {
		t.Helper()
		r, err := s.GetRepoByName(ctx, dbx, "m")
		if err != nil {
			t.Fatal(err)
		}
		return r.MirrorSyncedAt.Time, r.MirrorSyncedAt.Valid, r.MirrorError, r.UpdatedAt
	}
	_, synced, msg, updated := get()
	if synced || msg != "" {
		t.Fatalf("new repository: synced %v, error %q; want neither", synced, msg)
	}

	if err := s.SetRepoMirrorSyncByID(ctx, dbx, m.ID, "first failure"); err != nil {
		t.Fatal(err)
	}
	if _, synced, msg, _ := get(); synced || msg != "first failure" {
		t.Errorf("after a failure: synced %v, error %q", synced, msg)
	}

	if err := s.SetRepoMirrorSyncByID(ctx, dbx, m.ID, ""); err != nil {
		t.Fatal(err)
	}
	at, synced, msg, _ := get()
	if !synced || msg != "" {
		t.Errorf("after a success: synced %v, error %q", synced, msg)
	}

	if err := s.SetRepoMirrorSyncByID(ctx, dbx, m.ID, "second failure"); err != nil {
		t.Fatal(err)
	}
	again, synced, msg, updatedAfter := get()
	if !synced || !again.Equal(at) || msg != "second failure" {
		t.Errorf("after another failure: synced %v at %v (was %v), error %q", synced, again, at, msg)
	}
	if !updatedAfter.Equal(updated) {
		t.Errorf("updated_at moved from %v to %v", updated, updatedAfter)
	}
}

// The error of a sync is what git and the remote wrote, which need not be
// UTF-8 and can hold NUL bytes. Postgres refuses both in a TEXT column, so
// they are cleaned rather than leave the last outcome unrecorded. Runs on
// Postgres when SOFT_SERVE_DB_DRIVER is "postgres".
func TestSetRepoMirrorSyncByIDHostileText(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB
	m, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "m", UserID: owner, Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"fatal: \xff\xfe bad bytes", "fatal: \uFFFD bad bytes"},
		{"nul\x00 inside", "nul inside"},
		{"\x00", "\uFFFD"},
	} {
		if err := s.SetRepoMirrorSyncByID(ctx, dbx, m.ID, tc.in); err != nil {
			t.Fatalf("SetRepoMirrorSyncByID(%q): %v", tc.in, err)
		}
		r, err := s.GetRepoByName(ctx, dbx, "m")
		if err != nil {
			t.Fatal(err)
		}
		if r.MirrorError != tc.want {
			t.Errorf("stored %q as %q, want %q", tc.in, r.MirrorError, tc.want)
		}
	}
}

func TestRepoStore(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB

	// Names are sanitised: no leading slash, no .git suffix.
	if _, err := s.CreateRepo(ctx, dbx, models.Repo{
		Name:        "/team/app.git",
		UserID:      owner,
		ProjectName: "App",
		Description: "an app",
		Private:     true,
		Hidden:      true,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepoByName(ctx, dbx, "team/app")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "team/app" || r.ProjectName != "App" || r.Description != "an app" ||
		!r.Private || !r.Hidden || r.Mirror || r.Anonymous || r.UserID != owner {
		t.Errorf("GetRepoByName(team/app) = %+v", r)
	}
	if got, err := s.GetRepoByID(ctx, dbx, r.ID); err != nil || got.Name != "team/app" {
		t.Errorf("GetRepoByID(%d) = %+v, %v", r.ID, got, err)
	}
	if _, err := s.CreateRepo(ctx, dbx, models.Repo{
		Name:   "team/app",
		UserID: owner,
	}); !errors.Is(err, db.ErrDuplicateKey) {
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
	if m, err := s.GetRepoByName(ctx, dbx, "team/app"); err != nil ||
		m.Description != "new description" || m.ProjectName != "New App" || m.Private || m.Hidden || !m.Mirror {
		t.Errorf("GetRepoByName() after the setters = %+v, %v", m, err)
	}

	// Anonymous repositories are owned by someone but don't count against
	// their limit.
	if _, err := s.CreateRepo(ctx, dbx, models.Repo{Name: "anon", UserID: owner, Anonymous: true}); err != nil {
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

// Each flag of CreateRepo lands in its own column: one repository per flag,
// with only that flag set.
func TestCreateRepoColumns(t *testing.T) {
	ctx, dbx, s := newStoreTestDB(t)
	const owner = 100 // see newWebhookTestDB

	type flags struct{ private, hidden, mirror, anonymous bool }
	for name, want := range map[string]flags{
		"p": {private: true},
		"h": {hidden: true},
		"m": {mirror: true},
		"a": {anonymous: true},
	} {
		if _, err := s.CreateRepo(ctx, dbx, models.Repo{
			Name:        name,
			UserID:      owner,
			ProjectName: "P-" + name,
			Description: "D-" + name,
			Private:     want.private,
			Hidden:      want.hidden,
			Mirror:      want.mirror,
			Anonymous:   want.anonymous,
		}); err != nil {
			t.Fatal(err)
		}
		r, err := s.GetRepoByName(ctx, dbx, name)
		if err != nil {
			t.Fatal(err)
		}
		got := flags{private: r.Private, hidden: r.Hidden, mirror: r.Mirror, anonymous: r.Anonymous}
		if got != want || r.ProjectName != "P-"+name || r.Description != "D-"+name || r.UserID != owner {
			t.Errorf("repo %q = %+v, want flags %+v", name, r, want)
		}
	}
}

// GetRepoWithCollabByName returns the repository with the caller's
// collaborator level, ok only when the caller has a row, and not found for
// a missing name.
func TestGetRepoWithCollabByName(t *testing.T) {
	ctx, dbx := newWebhookTestDB(t)
	s := &Store{}
	if err := s.CreateUser(ctx, dbx, "alice", false, nil); err != nil {
		t.Fatal(err)
	}
	alice, err := s.FindUserByUsername(ctx, dbx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddCollabByUsernameAndRepo(ctx, dbx, "alice", "repo7", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	m, level, ok, err := s.GetRepoWithCollabByName(ctx, dbx, "repo7", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.ID, int64(7); got != want {
		t.Fatalf("m.ID = %v, want %v", got, want)
	}
	if got := m.Name; got != "repo7" {
		t.Fatalf("m.Name = %v, want %v", got, "repo7")
	}
	if !ok {
		t.Fatal("not true: ok")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}

	m, _, ok, err = s.GetRepoWithCollabByName(ctx, dbx, "repo8", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.ID, int64(8); got != want {
		t.Fatalf("m.ID = %v, want %v", got, want)
	}
	if ok {
		t.Fatal("not true: !ok")
	}

	_, _, ok, err = s.GetRepoWithCollabByName(ctx, dbx, "repo7", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("not true: !ok")
	}

	_, _, _, err = s.GetRepoWithCollabByName(ctx, dbx, "missing", alice.ID)
	if !errors.Is(err, db.ErrRecordNotFound) {
		t.Fatal("not true: errors.Is(err, db.ErrRecordNotFound)")
	}
}
