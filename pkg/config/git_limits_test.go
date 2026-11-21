package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestGitMaxConnectionsPerIP(t *testing.T) {
	is := is.New(t)
	// Ignore any value exported in the environment running the tests.
	t.Setenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP", "")
	is.NoErr(os.Unsetenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP"))
	cfg := DefaultConfig()
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.Git.MaxConnectionsPerIP, 8)
	// A share of the total, so that one address cannot take every slot.
	is.True(cfg.Git.MaxConnectionsPerIP > 0 && cfg.Git.MaxConnectionsPerIP < cfg.Git.MaxConnections)

	t.Setenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP", "3")
	cfg = DefaultConfig()
	is.NoErr(parseEnv(cfg))
	is.Equal(cfg.Git.MaxConnectionsPerIP, 3)
	// Hooks re-read the config from the environment.
	is.True(slices.Contains(cfg.Environ(), "SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP=3"))
	is.NoErr(os.Unsetenv("SOFT_SERVE_GIT_MAX_CONNECTIONS_PER_IP"))

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	is.NoErr(os.WriteFile(path, []byte("git:\n  max_connections_per_ip: 0\n"), 0o600))
	cfg = DefaultConfig()
	cfg.DataPath = dir
	is.NoErr(parseFile(cfg, path))
	is.Equal(cfg.Git.MaxConnectionsPerIP, 0)

	// The generated config file keeps the value.
	cfg.Git.MaxConnectionsPerIP = 5
	out := filepath.Join(dir, "written.yaml")
	is.NoErr(writeConfig(cfg, out))
	got := DefaultConfig()
	got.DataPath = dir
	is.NoErr(parseFile(got, out))
	is.Equal(got.Git.MaxConnectionsPerIP, 5)
}

func TestGitMaxConnectionsPerIPValidate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.Git.MaxConnectionsPerIP = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "git.max_connections_per_ip") {
		t.Errorf("Validate() = %v, want an error naming git.max_connections_per_ip", err)
	}
	cfg.Git.MaxConnectionsPerIP = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_connections_per_ip 0: Validate() = %v", err)
	}
}
