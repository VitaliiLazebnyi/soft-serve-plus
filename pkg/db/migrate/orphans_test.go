package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// A SQLite database used with foreign keys off, as a custom data source left
// them before db.Open enforced them, can hold rows that refer to deleted
// rows. Upgrading such a database from version 3 failed in migration 4,
// which copies the deliveries table, and the server did not start. The rows
// are now removed first, the repository of a deleted owner going to the
// first admin, and every migration runs.
func TestMigrateRemovesSqliteOrphans(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	var logs bytes.Buffer
	ctx = log.WithContext(ctx, log.New(&logs))
	path := filepath.Join(t.TempDir(), "soft.db")
	dbx, err := db.Open(ctx, "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:3]
	if err := Migrate(ctx, dbx); err != nil {
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
		`INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (100, 'ssh-ed25519 AAAA bob', CURRENT_TIMESTAMP)`,
		`INSERT INTO access_tokens (token, name, user_id, updated_at) VALUES ('t', 'n', 100, CURRENT_TIMESTAMP)`,
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

	migrations = all
	if err := Migrate(ctx, dbx); err != nil {
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
	for _, s := range []string{"table=public_keys rows=1", "table=webhook_deliveries rows=1", "table=webhooks rows=1", "repos=1"} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("logs = %q, want %q", logs.String(), s)
		}
	}

	// Down to version 3 and up again keeps every row: the table copies of
	// migration 4 and the column changes of the others hold with foreign
	// keys enforced.
	for range len(all) - 3 {
		if err := Rollback(ctx, dbx); err != nil {
			t.Fatalf("Rollback() => %v", err)
		}
	}
	assertNoOrphans(t, ctx, dbx)
	if err := Migrate(ctx, dbx); err != nil {
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
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
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
		if err := Rollback(ctx, dbx); err != nil {
			t.Fatalf("Rollback() => %v", err)
		}
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	if after := snapshot(); after != before {
		t.Errorf("rows after down to 3 and up:\n%s\nwant:\n%s", after, before)
	}
	if dbx.DriverName() == "sqlite" {
		assertNoOrphans(t, ctx, dbx)
	}
}
