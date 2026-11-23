package migrate

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// The settings rows that recorded which keys the admin key sync attached
// become public_keys.synced_user_id, and the rows go. Rolling back writes
// them again, and migrating again carries them over again. The test runs on
// Postgres when SOFT_SERVE_DB_DRIVER is "postgres".
func TestPublicKeySyncedUser(t *testing.T) {
	t.Parallel()
	ctx, env := context.TODO(), testEnv(config.DefaultConfig())
	dbx, err := dbtest.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Later migrations stay out of the test so that Rollback undoes this one.
	all := migrations

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	list := all[:publicKeySyncedUser.Version-1]
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() to %d => %v", publicKeySyncedUser.Version-1, err)
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'a', true, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (101, 'b', false, CURRENT_TIMESTAMP)`)
	for _, k := range []struct {
		key  string
		user int
	}{
		{"synced", 100}, // attached by the sync
		{"moved", 101},  // attached by the sync to 100, since moved by hand
		{"hand", 100},   // added by hand
		{"garbled", 100},
		{"padded", 100},
	} {
		mustExec(`INSERT INTO public_keys (user_id, public_key, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, k.user, k.key)
	}
	for key, value := range map[string]string{
		"synced":  "100",
		"moved":   "100",
		"garbled": "1x",
		"padded":  "0100",
		"stale":   "100", // its key was deleted
	} {
		mustExec(`INSERT INTO settings ("key", value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "initial_admin_key:"+key, value)
	}

	syncedUsers := func() map[string]int64 {
		t.Helper()
		var rows []struct {
			Key    string        `db:"public_key"`
			Synced sql.NullInt64 `db:"synced_user_id"`
		}
		if err := dbx.SelectContext(ctx, &rows, `SELECT public_key, synced_user_id FROM public_keys`); err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, r := range rows {
			if r.Synced.Valid {
				got[r.Key] = r.Synced.Int64
			}
		}
		return got
	}
	settings := func() map[string]string {
		t.Helper()
		var rows []struct {
			Key   string `db:"key"`
			Value string `db:"value"`
		}
		if err := dbx.SelectContext(ctx, &rows, `SELECT "key", value FROM settings`); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range rows {
			got[r.Key] = r.Value
		}
		// The orphan check's marker, on SQLite, is not under test.
		delete(got, orphansCheckedKey)
		return got
	}
	wantSynced := map[string]int64{"synced": 100, "moved": 100, "padded": 100}
	// The other settings, such as anon_access, stay as they are.
	wantSettings := settings()
	for k := range wantSettings {
		if strings.HasPrefix(k, "initial_admin_key:") {
			delete(wantSettings, k)
		}
	}

	list = all[:publicKeySyncedUser.Version]
	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	if got := syncedUsers(); !maps.Equal(got, wantSynced) {
		t.Errorf("synced users = %v, want %v", got, wantSynced)
	}
	if got := settings(); !maps.Equal(got, wantSettings) {
		t.Errorf("settings = %v, want %v", got, wantSettings)
	}

	if err := rollback(ctx, dbx, env, list); err != nil {
		t.Fatalf("Rollback() => %v", err)
	}
	if err := dbx.GetContext(ctx, new(int), `SELECT COUNT(synced_user_id) FROM public_keys`); err == nil {
		t.Error("the column is still there after the rollback")
	}
	wantRolledBack := maps.Clone(wantSettings)
	wantRolledBack["initial_admin_key:synced"] = "100"
	wantRolledBack["initial_admin_key:moved"] = "100"
	wantRolledBack["initial_admin_key:padded"] = "100"
	if got := settings(); !maps.Equal(got, wantRolledBack) {
		t.Errorf("settings after rollback = %v, want %v", got, wantRolledBack)
	}

	if err := migrate(ctx, dbx, env, list); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	if got := syncedUsers(); !maps.Equal(got, wantSynced) {
		t.Errorf("synced users after migrating again = %v, want %v", got, wantSynced)
	}
	if got := settings(); !maps.Equal(got, wantSettings) {
		t.Errorf("settings after migrating again = %v, want %v", got, wantSettings)
	}
}
