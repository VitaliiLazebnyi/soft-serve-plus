package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// none, create no log file where SOFT_SERVE_LOG_PATH says (R60, CFG-9).
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
