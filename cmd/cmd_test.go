package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
