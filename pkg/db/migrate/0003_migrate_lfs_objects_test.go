package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// The LFS objects migration applies to a database that already holds a
// repository with recorded objects, and moves each object from the old
// OID[:2]/OID[2:4]/OID[4:] path to OID[:2]/OID[2:4]/OID. Its per-repository
// query used "?" placeholders, which Postgres rejected. The test runs on
// Postgres when SOFT_SERVE_DB_DRIVER is "postgres".
func TestMigrateLfsObjects(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := config.WithContext(context.TODO(), cfg)
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:migrateLfsObjectsVersion-1]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() before lfs objects => %v", err)
	}
	migrations = all[:migrateLfsObjectsVersion]

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', false, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (1, 'r', 'r', '', false, false, false, 100, CURRENT_TIMESTAMP)`)
	const oid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mustExec(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (1, ?, 4, CURRENT_TIMESTAMP)`, oid)

	objs := filepath.Join(cfg.DataPath, "lfs", "1", "objects")
	oldpath := filepath.Join(objs, badRelativePath(oid))
	newpath := filepath.Join(objs, goodRelativePath(oid))
	if err := os.MkdirAll(filepath.Dir(oldpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldpath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	if _, err := os.Stat(oldpath); !os.IsNotExist(err) {
		t.Errorf("old object path still exists: %v", err)
	}
	if b, err := os.ReadFile(newpath); err != nil || string(b) != "data" {
		t.Errorf("new object path = %q, %v; want %q", b, err, "data")
	}
}
