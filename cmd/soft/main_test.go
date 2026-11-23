package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
)

func TestIsHookCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"hook", "pre-receive"}, true},
		{[]string{"hook", "update", "refs/heads/main", "a", "b"}, true},
		{[]string{"hook", "post-update", "refs/heads/main"}, true},
		{[]string{"hook"}, true},
		{[]string{"serve"}, false},
		{[]string{"admin", "migrate"}, false},
		{nil, false},
		{[]string{"no-such-command"}, false},
	} {
		if got := isHookCommand(tc.args); got != tc.want {
			t.Errorf("isHookCommand(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestManPageDateFollowsSourceDateEpoch checks that the man page is dated by
// SOURCE_DATE_EPOCH, so a release rebuilt on another day gives the same page.
func TestManPageDateFollowsSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000") // 2023-11-14T22:13:20Z
	var out bytes.Buffer
	manCmd.SetOut(&out)
	t.Cleanup(func() { manCmd.SetOut(nil) })
	if err := manCmd.RunE(manCmd, nil); err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	if !strings.HasPrefix(first, ".TH SOFT 1 ") || !strings.Contains(first, `"2023-11-14"`) {
		t.Errorf("man page heading = %q, want the SOURCE_DATE_EPOCH date 2023-11-14", first)
	}
}

func TestSourceDate(t *testing.T) {
	t.Parallel()
	if d, err := sourceDate("1700000000"); err != nil || !d.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("sourceDate(1700000000) = %v, %v", d, err)
	}
	if _, err := sourceDate("not-a-number"); err == nil {
		t.Error("sourceDate accepted an invalid SOURCE_DATE_EPOCH")
	}
	if d, err := sourceDate(""); err != nil || time.Since(d) > time.Minute {
		t.Errorf("sourceDate(\"\") = %v, %v; want now", d, err)
	}
}

// Commands that do not act on the server's data read no server config and
// open no server log: --version, --help and man succeed in a directory whose
// data/config.yaml the server would refuse, and, with a usable config or
// none, create no log file where SOFT_SERVE_LOG_PATH says.
func TestNonServerCommandsLoadNoConfig(t *testing.T) {
	for name, file := range map[string]string{
		"refused config": "db:\n  driver: mysql\n",
		"usable config":  "name: usable\n",
		"no config":      "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if file != "" {
				if err := os.MkdirAll("data", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join("data", "config.yaml"), []byte(file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, k := range []string{"SOFT_SERVE_DATA_PATH", "SOFT_SERVE_CONFIG_LOCATION"} {
				t.Setenv(k, "")
				if err := os.Unsetenv(k); err != nil {
					t.Fatal(err)
				}
			}
			logDir := filepath.Join(dir, "logs")
			t.Setenv("SOFT_SERVE_LOG_PATH", filepath.Join(logDir, "soft.log"))

			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			t.Cleanup(func() {
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
				rootCmd.SetArgs(nil)
			})
			for _, args := range [][]string{{"--version"}, {"--help"}, {"man"}} {
				out.Reset()
				if code := run(context.Background(), args); code != 0 {
					t.Errorf("soft %v exited %d: %s", args, code, out.String())
				}
			}
			if _, err := os.Stat(logDir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("soft --version, --help and man created the server log directory %s: %v", logDir, err)
			}
			if name == "no config" {
				if _, err := os.Stat("data"); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("they created the data directory: %v", err)
				}
			}
		})
	}
}

// loadServerConfig loads the config from the config file and the
// environment, opens the log it names and makes it the default logger; the
// log file is closed with the command's Env.
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
	// The config carries main's version, which webhook requests name in
	// their User-Agent.
	if cfg.Version == "" || cfg.Version != Version {
		t.Errorf("config Version = %q, want main's Version %q", cfg.Version, Version)
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
