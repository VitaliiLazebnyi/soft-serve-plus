package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
)

// loadServerConfig loads the config from the config file and the
// environment, opens the log it names and makes it the default logger; the
// log file is closed with the command's Env (R60).
func TestLoadServerConfig(t *testing.T) {
	old := log.Default()
	t.Cleanup(func() { log.SetDefault(old) })
	dp := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", dp)
	t.Setenv("SOFT_SERVE_CONFIG_LOCATION", "")
	t.Setenv("SOFT_SERVE_LOG_PATH", "")
	t.Setenv("SOFT_SERVE_NAME", "from env")
	if err := os.WriteFile(filepath.Join(dp, "config.yaml"), []byte("name: from file\nlog:\n  path: soft.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, logger, closeLog, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.Name != "from env" || cfg.Log.Path != filepath.Join(dp, "soft.log") {
		t.Fatalf("config = %+v, want the file's log path and the environment's name", cfg)
	}
	if log.Default() != logger {
		t.Error("the loaded logger is not the default one")
	}
	// The command logs on the logger it was given: with the default one
	// replaced, it still logs to the file.
	log.SetDefault(log.New(io.Discard))
	logger.Info("logged to the file")
	if closeLog == nil {
		t.Fatal("no function to close the log file")
	}
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Log.Path)
	if err != nil || !strings.Contains(string(data), "logged to the file") {
		t.Errorf("log file = %q, %v; want the logged line", data, err)
	}

	// A config the server cannot use is an error.
	if err := os.WriteFile(filepath.Join(dp, "config.yaml"), []byte("log:\n  format: pretty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadServerConfig(); err == nil || !strings.Contains(err.Error(), "log.format") {
		t.Errorf("loadServerConfig() = %v, want a log.format error", err)
	}
}
