package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Migration 3 moves LFS objects stored under the old path to the new one. Its
// query binds its parameter for the driver, so it also runs on Postgres.
func TestMigrateLfsObjectsMovesObjects(t *testing.T) {
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
		t.Fatal(err)
	}
	migrations = all

	oid := strings.Repeat("ab", 32)
	mustExec(t, dbx, `INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (5, 'repo', '', '', false, false, false, 1, CURRENT_TIMESTAMP)`)
	mustExec(t, dbx, `INSERT INTO lfs_objects (oid, size, repo_id, updated_at) VALUES (?, 1, 5, CURRENT_TIMESTAMP)`, oid)

	objs := filepath.Join(cfg.DataPath, "lfs", "5", "objects")
	oldpath := filepath.Join(objs, badRelativePath(oid))
	if err := os.MkdirAll(filepath.Dir(oldpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldpath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
	if _, err := os.Stat(filepath.Join(objs, goodRelativePath(oid))); err != nil {
		t.Errorf("object not moved: %v", err)
	}
}

// Migrations that read the server configuration fail with an error, rather
// than panic, when the context carries none.
func TestMigrateWithoutConfig(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); !errors.Is(err, errNoConfig) {
		t.Errorf("Migrate() without config => %v, want %v", err, errNoConfig)
	}
}
