package migrate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// openLegacySqlite opens a sqlite database holding a Soft Serve v0.6 "user"
// table with the given users, which the first migration imports.
func openLegacySqlite(t *testing.T, ctx context.Context, users ...ownerRow) *db.DB {
	t.Helper()
	dbx, err := dbtest.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbx, `CREATE TABLE user (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		admin BOOLEAN NOT NULL,
		updated_at DATETIME NOT NULL
	)`)
	for _, u := range users {
		mustExec(t, dbx, `INSERT INTO user (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
			u.Username, u.Admin)
	}
	return dbx
}

func TestFirstMigrationSeedsAdminKeysToAdminNotUserOne(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	// alice gets id 1 and is not an admin.
	dbx := openLegacySqlite(t, ctx,
		ownerRow{Username: "alice", Admin: false},
		ownerRow{Username: "admin", Admin: true},
	)

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	validate(t, cfg)
	if err := migrate(ctx, dbx, testEnv(cfg), migrations); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("key owner = %+v, %v; want admin", o, ok)
	}
}

func TestFirstMigrationCreatesAdminForKeysWhenLegacyUsersHaveNone(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openLegacySqlite(t, ctx, ownerRow{Username: "alice", Admin: false})

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	validate(t, cfg)
	if err := migrate(ctx, dbx, testEnv(cfg), migrations); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("key owner = %+v, %v; want admin", o, ok)
	}
}

func TestFirstMigrationDoesNotSeedAdminKeysToNonAdminNamedAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	// "admin" is a regular account and another admin exists.
	dbx := openLegacySqlite(t, ctx,
		ownerRow{Username: "admin", Admin: false},
		ownerRow{Username: "bob", Admin: true},
	)

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	validate(t, cfg)
	if err := migrate(ctx, dbx, testEnv(cfg), migrations); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); ok {
		t.Fatalf("key attached to %+v; want it left out", o)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM users WHERE username = 'admin' AND admin = false"); n != 1 {
		t.Errorf("admin was promoted")
	}
}

// openLegacySqliteWithRepos opens a sqlite database with foreign keys
// enforced, as the default data source does, holding a Soft Serve v0.6
// schema: users, keys, repositories and collaborators. The legacy tables
// have no foreign keys, so they can hold rows that refer to missing users or
// repositories.
func openLegacySqliteWithRepos(t *testing.T, ctx context.Context, users ...ownerRow) *db.DB {
	t.Helper()
	dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "legacy.db")+"?_pragma=foreign_keys(1)", db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	mustExec(t, dbx, `CREATE TABLE user (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		admin BOOLEAN NOT NULL,
		updated_at DATETIME NOT NULL
	)`)
	mustExec(t, dbx, `CREATE TABLE public_key (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		public_key TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL
	)`)
	mustExec(t, dbx, `CREATE TABLE repo (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		project_name TEXT NOT NULL,
		description TEXT NOT NULL,
		private BOOLEAN NOT NULL,
		mirror BOOLEAN NOT NULL,
		hidden BOOLEAN NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL
	)`)
	mustExec(t, dbx, `CREATE TABLE collab (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		repo_id INTEGER NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL,
		UNIQUE (user_id, repo_id)
	)`)
	for _, u := range users {
		mustExec(t, dbx, `INSERT INTO user (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
			u.Username, u.Admin)
	}
	return dbx
}

func addLegacyRepo(t *testing.T, dbx *db.DB, name string) {
	t.Helper()
	mustExec(t, dbx, `INSERT INTO repo (name, project_name, description, private, mirror, hidden, updated_at)
		VALUES (?, '', '', false, false, false, CURRENT_TIMESTAMP)`, name)
}

func TestFirstMigrationGivesLegacyReposToAdminWhenLegacyUsersHaveNone(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openLegacySqliteWithRepos(t, ctx, ownerRow{Username: "alice", Admin: false})
	addLegacyRepo(t, dbx, "repo1")

	if err := migrate(ctx, dbx, testEnv(config.DefaultConfig()), migrations); err != nil {
		t.Fatal(err)
	}

	var owner ownerRow
	if err := dbx.GetContext(context.Background(), &owner, `SELECT users.username, users.admin
		FROM users INNER JOIN repos ON users.id = repos.user_id WHERE repos.name = 'repo1'`); err != nil {
		t.Fatal(err)
	}
	if owner.Username != "admin" || !owner.Admin {
		t.Fatalf("repo owner = %+v; want admin", owner)
	}
}

func TestFirstMigrationLeavesOutOrphanedLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	dbx := openLegacySqliteWithRepos(t, ctx,
		ownerRow{Username: "admin", Admin: true},
		ownerRow{Username: "alice", Admin: false},
	)
	addLegacyRepo(t, dbx, "repo1")
	alice, missing := newAuthorizedKey(t), newAuthorizedKey(t)
	mustExec(t, dbx, `INSERT INTO public_key (user_id, public_key, updated_at) VALUES (2, ?, CURRENT_TIMESTAMP)`, alice)
	mustExec(t, dbx, `INSERT INTO public_key (user_id, public_key, updated_at) VALUES (99, ?, CURRENT_TIMESTAMP)`, missing)
	// alice on repo1, a missing user on repo1 and alice on a missing repo.
	mustExec(t, dbx, `INSERT INTO collab (user_id, repo_id, updated_at) VALUES
		(2, 1, CURRENT_TIMESTAMP), (99, 1, CURRENT_TIMESTAMP), (2, 99, CURRENT_TIMESTAMP)`)

	if err := migrate(ctx, dbx, testEnv(config.DefaultConfig()), migrations); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, alice); !ok || o.Username != "alice" {
		t.Errorf("alice's key owner = %+v, %v; want alice", o, ok)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM public_keys"); n != 1 {
		t.Errorf("imported %d public keys; want 1", n)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM collabs WHERE user_id = 2 AND repo_id = 1"); n != 1 {
		t.Errorf("alice's collaboration on repo1 was not imported")
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM collabs"); n != 1 {
		t.Errorf("imported %d collaborators; want 1", n)
	}
	var violations []struct {
		Table  string `db:"table"`
		RowID  int64  `db:"rowid"`
		Parent string `db:"parent"`
		FKID   int64  `db:"fkid"`
	}
	if err := dbx.SelectContext(context.Background(), &violations, "PRAGMA foreign_key_check"); err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("foreign key violations: %+v", violations)
	}
}

func TestFirstMigrationFailsClearlyWithoutAnAdminForLegacyRepos(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	// "admin" is taken by a regular account and nobody is an admin.
	dbx := openLegacySqliteWithRepos(t, ctx, ownerRow{Username: "admin", Admin: false})
	addLegacyRepo(t, dbx, "repo1")

	err := migrate(ctx, dbx, testEnv(config.DefaultConfig()), migrations)
	if err == nil || !strings.Contains(err.Error(), "no admin user to own the imported repositories") {
		t.Fatalf("Migrate() => %v; want an error about the missing admin", err)
	}
}

// A database migrated from Soft Serve v0.6 tables may hold admin keys the
// import brought in without a record, so the first migration does not mark
// seeded keys as recorded there, and the boot warning about unrecorded admin
// keys still covers it. A database created from nothing is marked.
func TestFirstMigrationMarksRecordedKeysOnlyWithoutLegacyTables(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	marked := func(dbx *db.DB) bool {
		t.Helper()
		var n int
		if err := dbx.GetContext(context.Background(), &n, dbx.Rebind(`SELECT COUNT(*) FROM settings WHERE "key" = ?`), adminKeysRecordedKey); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	legacy := openLegacySqlite(t, ctx, ownerRow{Username: "admin", Admin: true})
	mustExec(t, legacy, `CREATE TABLE public_key (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		public_key TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL
	)`)
	old := newAuthorizedKey(t)
	mustExec(t, legacy, `INSERT INTO public_key (user_id, public_key, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)`, old)
	cfg := config.DefaultConfig()
	if err := migrate(ctx, legacy, testEnv(cfg), migrations); err != nil {
		t.Fatal(err)
	}
	if marked(legacy) {
		t.Error("a database imported from v0.6 is marked as recording its seeded keys")
	}
	if o, ok := ownerOf(t, legacy, old); !ok || o.Username != "admin" {
		t.Fatalf("imported key owner = %+v, %v; want admin", o, ok)
	}
	if logs := bootLogs(t, ctx, cfg, legacy); !strings.Contains(logs, "WARN") {
		t.Errorf("logs = %q; want a warning about the imported admin key", logs)
	}

	fresh, err := dbtest.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, fresh, testEnv(cfg), migrations); err != nil {
		t.Fatal(err)
	}
	if !marked(fresh) {
		t.Error("a database created by this version is not marked as recording its seeded keys")
	}
}
