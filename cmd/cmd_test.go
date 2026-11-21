package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/spf13/cobra"
)

// CloseDBOnError closes the database when the wrapped RunE fails, since
// cobra skips PersistentPostRunE then, and leaves it open otherwise.
func TestCloseDBOnError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runErr     error
		wantClosed bool
	}{
		{name: "error", runErr: errors.New("boom"), wantClosed: true},
		{name: "success", runErr: nil, wantClosed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = dbx.Close() })

			c := &cobra.Command{}
			c.SetContext(db.WithContext(ctx, dbx))
			run := CloseDBOnError(func(*cobra.Command, []string) error { return tc.runErr })
			if err := run(c, nil); !errors.Is(err, tc.runErr) {
				t.Fatalf("run returned %v, want %v", err, tc.runErr)
			}

			err = dbx.PingContext(ctx)
			closed := err != nil && strings.Contains(err.Error(), "closed")
			if closed != tc.wantClosed {
				t.Fatalf("database ping returned %v, want closed=%v", err, tc.wantClosed)
			}
		})
	}
}

// InitBackendContext applies the db pool settings from the config to the
// database it opens.
func TestInitBackendContextAppliesPoolLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.MaxOpenConns = 7
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), cfg))
	if err := InitBackendContext(c, nil); err != nil {
		t.Fatal(err)
	}
	dbx := db.FromContext(c.Context())
	t.Cleanup(func() { _ = dbx.Close() })
	if got := dbx.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}
}

// InitBackendContext creates a missing data directory that only the
// server's user can open, since it holds private repositories and the
// database.
func TestInitBackendContextCreatesPrivateDataPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), cfg))
	if err := InitBackendContext(c, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.FromContext(c.Context()).Close() })
	fi, err := os.Stat(cfg.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("data path mode = %v, want %v", got, os.FileMode(0o700))
	}
}

// LoadConfig loads the config from the config file and the environment,
// opens the log it names, and puts both in the command's context; the log
// file is closed with the database (R60).
func TestLoadConfig(t *testing.T) {
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
	c := &cobra.Command{}
	c.SetContext(context.Background())
	if err := LoadConfig(c); err != nil {
		t.Fatal(err)
	}
	cfg := config.FromContext(c.Context())
	if cfg == nil || cfg.Name != "from env" || cfg.Log.Path != filepath.Join(dp, "soft.log") {
		t.Fatalf("config in the context = %+v, want the file's log path and the environment's name", cfg)
	}
	// The context carries the logger: with the default one replaced, the
	// command still logs to the file.
	log.SetDefault(log.New(io.Discard))
	log.FromContext(c.Context()).Info("logged to the file")
	if err := CloseDBContext(c, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Log.Path)
	if err != nil || !strings.Contains(string(data), "logged to the file") {
		t.Errorf("log file = %q, %v; want the logged line", data, err)
	}

	// A context that already holds a config keeps it.
	mine := config.DefaultConfig()
	c = &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), mine))
	if err := LoadConfig(c); err != nil {
		t.Fatal(err)
	}
	if config.FromContext(c.Context()) != mine {
		t.Error("LoadConfig replaced the context's config")
	}

	// A config the server cannot use is an error.
	if err := os.WriteFile(filepath.Join(dp, "config.yaml"), []byte("log:\n  format: pretty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = &cobra.Command{}
	c.SetContext(context.Background())
	if err := LoadConfig(c); err == nil || !strings.Contains(err.Error(), "log.format") {
		t.Errorf("LoadConfig() = %v, want a log.format error", err)
	}
}
