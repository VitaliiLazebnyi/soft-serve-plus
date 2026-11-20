package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
)

// setHookEnv sets what the server gives a hook: the data path and the hook
// log, with a broken config.yaml and a config variable that differs from
// the server's, which a hook must not read.
func setHookEnv(t *testing.T) (dataPath, logPath string) {
	t.Helper()
	dataPath = t.TempDir()
	logPath = filepath.Join(dataPath, "log", "hooks.log")
	t.Setenv("SOFT_SERVE_DATA_PATH", dataPath)
	t.Setenv("SOFT_SERVE_LOG_PATH", logPath)
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", "")
	t.Setenv("SOFT_SERVE_NAME", "from the environment")
	if err := os.WriteFile(filepath.Join(dataPath, "config.yaml"), []byte("ssh: {max_timeout: -5}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataPath, logPath
}

// A hook runs with the server's effective config from the hook config file,
// whatever config.yaml and the environment say (R45).
func TestLoadHookConfigReadsTheServersConfig(t *testing.T) {
	dataPath, logPath := setHookEnv(t)
	server := config.DefaultConfig()
	server.DataPath = dataPath
	server.Name = "the server"
	if err := server.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := hooks.WriteConfig(server); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hooks.ConfigEnv, hooks.ConfigPath(server))

	cfg, err := loadHookConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "the server" {
		t.Errorf("name %q, want the server's", cfg.Name)
	}
	if cfg.Log.Path != logPath {
		t.Errorf("log path %q, want %q", cfg.Log.Path, logPath)
	}
}

// A hook config that cannot be loaded is an error, with a config that logs
// to the hook log, so the caller can report it there.
func TestLoadHookConfigErrorLogsToHookLog(t *testing.T) {
	dataPath, logPath := setHookEnv(t)
	broken := filepath.Join(dataPath, "hook-config.yaml")
	if err := os.WriteFile(broken, []byte("ssh: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hooks.ConfigEnv, broken)

	cfg, err := loadHookConfig()
	if err == nil {
		t.Fatal("no error for a broken hook config")
	}
	if cfg.Log.Path != logPath {
		t.Errorf("log path %q, want the hook log %q", cfg.Log.Path, logPath)
	}
}

// Under a server that names no hook config, one from before it, a hook
// loads config.yaml and the environment as before, and a broken config.yaml
// is an error with a config that logs to the hook log.
func TestLoadHookConfigWithoutHookConfig(t *testing.T) {
	_, logPath := setHookEnv(t)
	t.Setenv(hooks.ConfigEnv, "")

	cfg, err := loadHookConfig()
	if err == nil {
		t.Fatal("no error for a broken config.yaml")
	}
	if cfg.Log.Path != logPath {
		t.Errorf("log path %q, want the hook log %q", cfg.Log.Path, logPath)
	}
}
