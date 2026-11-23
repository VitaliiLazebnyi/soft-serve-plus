package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// A SQLite database used with foreign keys off, as a custom data source left
// them before db.Open enforced them, can hold rows that refer to deleted
// rows. Upgrading such a database from version 3 failed in migration 4,
// which copies the deliveries table, and the server did not start. The rows
// are now removed first, the repository of a deleted owner going to the
// first admin, and every migration runs.
func TestMigrateRemovesSqliteOrphans(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	var logs bytes.Buffer
	env.Logger = log.New(&logs).WithPrefix("migrate")
	path := filepath.Join(t.TempDir(), "soft.db")
	dbx, err := db.Open(ctx, "sqlite", path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	all := migrations
	list := all[:3]
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() to 3 => %v", err)
	}

	// Write the orphans the way an old data source did: with foreign keys
	// off, which db.Open no longer allows.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck
	raw.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'bob', 0, CURRENT_TIMESTAMP)`,
		`INSERT INTO public_keys (id, user_id, public_key, updated_at) VALUES (5, 100, 'ssh-ed25519 AAAA bob', CURRENT_TIMESTAMP)`,
		`INSERT INTO access_tokens (id, token, name, user_id, updated_at) VALUES (6, 't', 'n', 100, CURRENT_TIMESTAMP)`,
		`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
			VALUES (7, 'kept', '', '', 0, 0, 0, 100, CURRENT_TIMESTAMP),
			       (8, 'gone', '', '', 0, 0, 0, 1, CURRENT_TIMESTAMP),
			       (9, 'live', '', '', 0, 0, 0, 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO collabs (user_id, repo_id, access_level, updated_at) VALUES (1, 8, 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO lfs_objects (oid, size, repo_id, updated_at) VALUES ('o', 1, 8, CURRENT_TIMESTAMP)`,
		`INSERT INTO lfs_locks (repo_id, user_id, path, updated_at) VALUES (8, 1, 'a', CURRENT_TIMESTAMP)`,
		`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
			VALUES (1, 8, 'u', '', 1, 1, CURRENT_TIMESTAMP), (2, 9, 'u', '', 1, 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO webhook_events (webhook_id, event) VALUES (1, 1), (2, 1)`,
		`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
			VALUES ('d1', 1, 1, 'u', 'POST', '', '{}', 200, '', ''),
			       ('d2', 2, 1, 'u', 'POST', '', '{}', 200, '', ''),
			       ('d3', 3, 1, 'u', 'POST', '', '{}', 200, '', '')`,
		// The deletes that left them behind.
		`DELETE FROM users WHERE id = 100`,
		`DELETE FROM repos WHERE id = 8`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close() //nolint:errcheck

	list = all
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	assertNoOrphans(t, ctx, dbx)

	counts := func() map[string]int {
		t.Helper()
		got := map[string]int{}
		for _, table := range []string{
			"public_keys", "access_tokens", "repos", "collabs", "lfs_objects",
			"lfs_locks", "webhooks", "webhook_events", "webhook_deliveries",
		} {
			var n int
			if err := dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM "+table); err != nil {
				t.Fatal(err)
			}
			got[table] = n
		}
		return got
	}
	// What stays: repositories 7 (given to the admin) and 9, and repository 9's webhook with its event and delivery.
	want := map[string]int{
		"public_keys": 0, "access_tokens": 0, "repos": 2, "collabs": 0, "lfs_objects": 0,
		"lfs_locks": 0, "webhooks": 1, "webhook_events": 1, "webhook_deliveries": 1,
	}
	if got := counts(); !maps.Equal(got, want) {
		t.Errorf("rows after migrating = %v, want %v", got, want)
	}
	var owner int64
	if err := dbx.GetContext(ctx, &owner, `SELECT user_id FROM repos WHERE name = 'kept'`); err != nil || owner != 1 {
		t.Errorf("owner of the repository of a deleted user = %d, %v; want the admin, 1", owner, err)
	}
	// Counts include the rows removed with them by cascade, and the ids of
	// removed keys, tokens and webhooks are named, as is every repository
	// given to the admin.
	for _, s := range []string{
		"table=public_keys rows=1 ids=[5]", "table=access_tokens rows=1 ids=[6]",
		"table=webhooks rows=1 ids=[1]", "table=webhook_events rows=1", "table=webhook_deliveries rows=2",
		"repo=kept former_user_id=100 user_id=1",
	} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("logs = %q, want %q", logs.String(), s)
		}
	}

	// Down to version 3 and up again keeps every row: the table copies of
	// migration 4 and the column changes of the others hold with foreign
	// keys enforced.
	for range len(all) - 3 {
		if err := rollback(ctx, dbx, env, list); err != nil {
			t.Fatalf("Rollback() => %v", err)
		}
	}
	assertNoOrphans(t, ctx, dbx)
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	assertNoOrphans(t, ctx, dbx)
	if got := counts(); !maps.Equal(got, want) {
		t.Errorf("rows after down and up = %v, want %v", got, want)
	}
}

func assertNoOrphans(t *testing.T, ctx context.Context, dbx *db.DB) {
	t.Helper()
	var rows []fkViolation
	if err := dbx.SelectContext(ctx, &rows, "PRAGMA foreign_key_check"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("foreign_key_check = %+v, want none", rows)
	}
}

// Down to version 3 and up again, on either driver, with rows in every table
// the migrations change: nothing is lost and no row is left referring to a
// missing one.
func TestMigrateDownAndUpKeepsRows(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'bob', false, CURRENT_TIMESTAMP)`,
		`INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (100, 'ssh-ed25519 AAAA bob', CURRENT_TIMESTAMP)`,
		`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
			VALUES (7, 'r', '', '', false, true, false, 100, CURRENT_TIMESTAMP)`,
		`INSERT INTO collabs (user_id, repo_id, access_level, updated_at) VALUES (100, 7, 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO lfs_objects (oid, size, repo_id, updated_at) VALUES ('o', 2000000000, 7, CURRENT_TIMESTAMP)`,
		`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
			VALUES (1, 7, 'u', '', 1, true, CURRENT_TIMESTAMP)`,
		`INSERT INTO webhook_events (webhook_id, event) VALUES (1, 1)`,
		`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
			VALUES ('d1', 1, 1, 'u', 'POST', '', '{}', 200, '', ''), ('d2', 1, 1, 'u', 'POST', '', '{}', 500, '', '')`,
	} {
		if _, err := dbx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		for _, q := range []string{
			`SELECT COUNT(*) FROM users`, `SELECT COUNT(*) FROM public_keys`, `SELECT COUNT(*) FROM repos`,
			`SELECT COUNT(*) FROM collabs`, `SELECT size FROM lfs_objects`, `SELECT COUNT(*) FROM webhook_events`,
			`SELECT COUNT(*) FROM webhook_deliveries`,
		} {
			var n int64
			if err := dbx.GetContext(ctx, &n, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			b.WriteString(q + " = " + strconv.FormatInt(n, 10) + "\n")
		}
		return b.String()
	}
	before := snapshot()

	n := len(migrations)
	for range n - 3 {
		if err := rollback(ctx, dbx, env, migrations); err != nil {
			t.Fatalf("Rollback() => %v", err)
		}
	}
	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	if after := snapshot(); after != before {
		t.Errorf("rows after down to 3 and up:\n%s\nwant:\n%s", after, before)
	}
	if dbx.DriverName() == "sqlite" {
		assertNoOrphans(t, ctx, dbx)
	}
}

// openSqliteFile opens an empty SQLite database at a file, and a raw
// connection to it with foreign keys off, as an old data source left them,
// to write orphans with.
func openSqliteFile(t *testing.T, ctx context.Context) (*db.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "soft.db")
	dbx, err := db.Open(ctx, "sqlite", path, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() }) //nolint:errcheck
	raw.SetMaxOpenConns(1)
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	return dbx, raw
}

func rawExec(t *testing.T, ctx context.Context, raw *sql.DB, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// With no admin to give a repository of a deleted owner to, the upgrade
// stops with an error that names the repository and says how to make a user
// an admin, with commands that work while it cannot start, and changes
// nothing. The repository's row used to be deleted, with
// its collaborators, webhooks and LFS rows, leaving its directory behind and
// its name unusable. Once a user is an admin, the upgrade goes through.
func TestMigrateRefusesOwnerlessReposWithoutAdmin(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, raw := openSqliteFile(t, ctx)
	all := migrations
	list := all[:3]
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatal(err)
	}
	rawExec(t, ctx, raw,
		`UPDATE users SET admin = 0`,
		`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'bob', 0, CURRENT_TIMESTAMP)`,
		`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
			VALUES (7, 'kept', '', '', 0, 0, 0, 100, CURRENT_TIMESTAMP)`,
		`INSERT INTO collabs (user_id, repo_id, access_level, updated_at) VALUES (1, 7, 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO public_keys (id, user_id, public_key, updated_at) VALUES (5, 100, 'ssh-ed25519 AAAA bob', CURRENT_TIMESTAMP)`,
		`DELETE FROM users WHERE id = 100`,
	)

	list = all
	err := migrate(ctx, dbx, env, list)
	if err == nil {
		t.Fatal("Migrate() without an admin => nil, want an error")
	}
	msg := err.Error()
	// The file as SQLite names it: symbolic links resolved on macOS, 8.3
	// short names kept on Windows.
	file, err := sqliteFilePath(ctx, dbx)
	if err != nil || file == "" {
		t.Fatalf("database file = %q, %v", file, err)
	}
	path := shellQuote(runtime.GOOS, file)
	note := shellNote(runtime.GOOS)
	for _, want := range []string{
		"kept (owner id 100)",
		"nothing was changed",
		"With the previous version running, before upgrading, as an admin with a key listed in initial_admin_keys: " +
			"ssh -p PORT HOST user set-admin USERNAME true.",
		"With the server stopped" + note + ": sqlite3 " + path + ` "UPDATE users SET admin = 1 WHERE username = 'USERNAME'".`,
		"With no users at all, stop the server and add an admin" + note + ": sqlite3 " + path +
			` "INSERT INTO users (username, admin, updated_at) VALUES ('admin', 1, CURRENT_TIMESTAMP)"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Migrate() without an admin => %q, want it to contain %q", msg, want)
		}
	}
	// soft has no user command; user set-admin is an SSH command.
	if strings.Contains(msg, "soft user") {
		t.Errorf("Migrate() without an admin => %q, names a soft user command, which does not exist", msg)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM repos WHERE name = 'kept' AND user_id = 100`: 1,
		`SELECT COUNT(*) FROM collabs`:                                     1,
		`SELECT COUNT(*) FROM public_keys`:                                 1,
		`SELECT COUNT(*) FROM migrations`:                                  3,
	} {
		var n int
		if err := dbx.GetContext(ctx, &n, q); err != nil || n != want {
			t.Errorf("%s = %d, %v; want %d", q, n, err, want)
		}
	}

	if _, err := dbx.ExecContext(ctx, `UPDATE users SET admin = 1 WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() with an admin => %v", err)
	}
	assertNoOrphans(t, ctx, dbx)
	var owner int64
	if err := dbx.GetContext(ctx, &owner, `SELECT user_id FROM repos WHERE name = 'kept'`); err != nil || owner != 1 {
		t.Errorf("owner of kept = %d, %v; want the admin, 1", owner, err)
	}
}

// A database checked clean, rolled back and given new orphans is checked
// again when it migrates up: the marker of the first check does not skip it.
func TestMigrateChecksOrphansAgainAfterRollback(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, raw := openSqliteFile(t, ctx)
	for range 2 {
		if err := migrate(ctx, dbx, env, migrations); err != nil {
			t.Fatal(err)
		}
	}
	var marked int
	if err := dbx.GetContext(ctx, &marked, `SELECT COUNT(*) FROM settings WHERE "key" = ?`, orphansCheckedKey); err != nil || marked != 1 {
		t.Fatalf("orphan check marker = %d, %v; want it written", marked, err)
	}
	if err := rollback(ctx, dbx, env, migrations); err != nil {
		t.Fatal(err)
	}
	rawExec(t, ctx, raw,
		`INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (999, 'ssh-ed25519 AAAA ghost', CURRENT_TIMESTAMP)`)

	if err := migrate(ctx, dbx, env, migrations); err != nil {
		t.Fatalf("Migrate() after rollback => %v", err)
	}
	assertNoOrphans(t, ctx, dbx)
}

// The message says which shell its commands are quoted for where that is
// not the plain one: on Windows, whose double quotes still let cmd.exe
// expand %NAME% and PowerShell $name, it names cmd.exe (tracker 322).
func TestShellNote(t *testing.T) {
	t.Parallel()
	if got := shellNote("windows"); got != " (in cmd.exe)" {
		t.Errorf("shellNote(windows) = %q, want it to name cmd.exe", got)
	}
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if got := shellNote(goos); got != "" {
			t.Errorf("shellNote(%s) = %q, want none", goos, got)
		}
	}
}

// A database path is quoted for the shell of the system the message is
// shown on, so the command it is pasted into opens that file.
func TestShellQuote(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		goos, path, want string
	}{
		{"linux", "/var/lib/soft-serve/soft-serve.db", "/var/lib/soft-serve/soft-serve.db"},
		{"linux", "/srv/my data/soft.db", "'/srv/my data/soft.db'"},
		{"linux", "/srv/bob's/soft.db", `'/srv/bob'\''s/soft.db'`},
		{"linux", "/srv/$HOME/soft.db", "'/srv/$HOME/soft.db'"},
		{"linux", "/srv/a`id`b/soft.db", "'/srv/a`id`b/soft.db'"},
		{"darwin", `/srv/x\y/soft.db`, `'/srv/x\y/soft.db'`},
		{"linux", "/srv/~me/soft.db", "'/srv/~me/soft.db'"},
		{"windows", `C:\ProgramData\soft-serve\soft-serve.db`, `C:\ProgramData\soft-serve\soft-serve.db`},
		{"windows", `C:\Users\RUNNER~1\AppData\Local\Temp\soft.db`, `"C:\Users\RUNNER~1\AppData\Local\Temp\soft.db"`},
		{"windows", `C:\Program Files\soft-serve\soft.db`, `"C:\Program Files\soft-serve\soft.db"`},
		{"windows", `C:\data\bob's\soft.db`, `"C:\data\bob's\soft.db"`},
		{"windows", `C:\data\$x\soft.db`, `"C:\data\$x\soft.db"`},
		{"windows", "C:\\data\\a`b\\soft.db", "\"C:\\data\\a`b\\soft.db\""},
		{"windows", `\\server\share\soft.db`, `\\server\share\soft.db`},
	} {
		if got := shellQuote(tt.goos, tt.path); got != tt.want {
			t.Errorf("shellQuote(%q, %q) = %s, want %s", tt.goos, tt.path, got, tt.want)
		}
	}
}
