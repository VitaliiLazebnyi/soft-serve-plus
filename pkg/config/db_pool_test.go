package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestDBPoolLimits(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_DB_MAX_OPEN_CONNS", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_DB_MAX_OPEN_CONNS"))
	cfg := DefaultConfig()
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.DB.MaxOpenConns, 0)

	t.Setenv("SOFT_SERVE_DB_MAX_OPEN_CONNS", "20")
	cfg = DefaultConfig()
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.DB.MaxOpenConns, 20)
	// Hooks re-read the config from the environment.
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_DB_MAX_OPEN_CONNS=20"))
	is.NoErr(os.Unsetenv("SOFT_SERVE_DB_MAX_OPEN_CONNS"))

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte("db:\n  max_open_conns: 10\n"), 0o600))
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.DB.MaxOpenConns, 10)
}

func TestDBPoolLimitsRejectNegative(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.MaxOpenConns = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "db.max_open_conns") {
		t.Errorf("Validate() = %v, want an error naming db.max_open_conns", err)
	}
}
