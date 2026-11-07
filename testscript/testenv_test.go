package testscript

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// The developer's SOFT_SERVE_* environment must not move the test server's
// data, listeners or admins elsewhere; only the database settings apply.
func TestApplyTestEnv(t *testing.T) {
	const testAdmin = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJG/rpTb2NLgzUBZOVt1lm/OkMEiSCnicSTHt2poYJ6k"
	data := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", filepath.Join(t.TempDir(), "real-server"))
	t.Setenv("SOFT_SERVE_NAME", "Real Server")
	t.Setenv("SOFT_SERVE_INITIAL_ADMIN_KEYS", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFTGWpI3MrM9e8yqZeMVKCH8ABrTtCtnQmmzuGXhqRJ real-admin")
	t.Setenv("SOFT_SERVE_SSH_LISTEN_ADDR", ":23231")
	t.Setenv("SOFT_SERVE_LFS_ENABLED", "false")
	t.Setenv("SOFT_SERVE_DB_DRIVER", "postgres")
	t.Setenv("SOFT_SERVE_DB_DATA_SOURCE", "postgres://postgres@db.example/postgres")

	cfg := config.DefaultConfig()
	cfg.DataPath = data
	cfg.Name = "Test Soft Serve"
	cfg.InitialAdminKeys = []string{testAdmin}
	cfg.SSH.ListenAddr = "127.0.0.1:1"
	cfg.LFS.Enabled = true

	if err := applyTestEnv(cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.DataPath != data {
		t.Errorf("data path = %q, want %q", cfg.DataPath, data)
	}
	if cfg.Name != "Test Soft Serve" {
		t.Errorf("name = %q, want the test's", cfg.Name)
	}
	if len(cfg.InitialAdminKeys) != 1 || cfg.InitialAdminKeys[0] != testAdmin {
		t.Errorf("initial admin keys = %q, want only the test's", cfg.InitialAdminKeys)
	}
	if cfg.SSH.ListenAddr != "127.0.0.1:1" {
		t.Errorf("ssh listen addr = %q, want the test's", cfg.SSH.ListenAddr)
	}
	if !cfg.LFS.Enabled {
		t.Error("lfs disabled by the environment")
	}
	if cfg.DB.Driver != "postgres" || cfg.DB.DataSource != "postgres://postgres@db.example/postgres" {
		t.Errorf("db = %q %q, want the environment's", cfg.DB.Driver, cfg.DB.DataSource)
	}
}
