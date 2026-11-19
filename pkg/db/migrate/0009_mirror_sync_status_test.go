package migrate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Repositories get a last sync time, empty until a sync succeeds, and a last
// error, empty by default, on existing rows too. Rolling back drops both,
// and migrating again adds them back. The test runs on Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres".
func TestMirrorSyncStatus(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Later migrations stay out of the test so that Rollback undoes this one.
	all := migrations
	t.Cleanup(func() { migrations = all })

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// A repository from before the migration.
	migrations = all[:mirrorSyncStatusVersion-1]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() to %d => %v", mirrorSyncStatusVersion-1, err)
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', false, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (1, 'r', 'r', '', false, true, false, 100, CURRENT_TIMESTAMP)`)

	migrations = all[:mirrorSyncStatusVersion]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	var row struct {
		SyncedAt sql.NullTime `db:"mirror_synced_at"`
		Error    string       `db:"mirror_error"`
	}
	read := func() {
		t.Helper()
		if err := dbx.GetContext(ctx, &row, `SELECT mirror_synced_at, mirror_error FROM repos WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if row.SyncedAt.Valid || row.Error != "" {
		t.Errorf("existing row = %+v, want no sync and no error", row)
	}
	mustExec(`UPDATE repos SET mirror_synced_at = CURRENT_TIMESTAMP, mirror_error = 'boom' WHERE id = 1`)
	read()
	if !row.SyncedAt.Valid || row.Error != "boom" {
		t.Errorf("updated row = %+v, want a sync time and the error", row)
	}

	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v", err)
	}
	if err := dbx.GetContext(ctx, &row, `SELECT mirror_synced_at, mirror_error FROM repos WHERE id = 1`); err == nil {
		t.Error("the columns are still there after the rollback")
	}
	var name string
	if err := dbx.GetContext(ctx, &name, `SELECT name FROM repos WHERE id = 1`); err != nil || name != "r" {
		t.Errorf("repository after rollback = %q, %v; want r", name, err)
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	read()
	if row.SyncedAt.Valid || row.Error != "" {
		t.Errorf("row after rollback and migrate = %+v, want no sync and no error", row)
	}
}
