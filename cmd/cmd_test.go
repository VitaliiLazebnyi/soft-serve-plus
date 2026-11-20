package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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

// testConfig returns a validated default config whose data path is dp.
func testConfig(t *testing.T, dp string) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The maintenance commands refuse a data directory or SQLite database that
// does not exist, and create neither, where they used to create an empty
// one and act on it (R46, CFG-7). serve still creates them.
func TestOpenBackendContextRefusesMissingDataPath(t *testing.T) {
	dp := filepath.Join(t.TempDir(), "data")
	c := &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), testConfig(t, dp)))
	err := OpenBackendContext(c, nil)
	if err == nil || !strings.Contains(err.Error(), "SOFT_SERVE_DATA_PATH") || !strings.Contains(err.Error(), dp) {
		t.Fatalf("OpenBackendContext() = %v, want an error naming the data directory and SOFT_SERVE_DATA_PATH", err)
	}
	if _, err := os.Stat(dp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing data directory was created: %v", err)
	}
}

func TestOpenBackendContextRefusesMissingDatabase(t *testing.T) {
	dp := t.TempDir()
	cfg := testConfig(t, dp)
	file := filepath.Join(dp, "soft-serve.db")
	c := &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), cfg))
	err := OpenBackendContext(c, nil)
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("OpenBackendContext() = %v, want an error naming %s", err, file)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing database was created: %v", err)
	}

	// Once serve has created it, a maintenance command opens it.
	c = &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), cfg))
	if err := InitBackendContext(c, nil); err != nil {
		t.Fatal(err)
	}
	if err := CloseDBContext(c, nil); err != nil {
		t.Fatal(err)
	}
	c = &cobra.Command{}
	c.SetContext(config.WithContext(context.Background(), cfg))
	if err := OpenBackendContext(c, nil); err != nil {
		t.Fatalf("OpenBackendContext() on an existing database = %v", err)
	}
	if err := CloseDBContext(c, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSqliteFile(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "soft serve.db")
	slash := filepath.ToSlash(abs)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash // file:///C:/...
	}
	for _, tc := range []struct{ dsn, want string }{
		{abs, abs},
		{abs + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", abs},
		{"file:" + (&url.URL{Path: slash}).EscapedPath() + "?cache=shared", abs},
		{"file://" + (&url.URL{Path: slash}).EscapedPath(), abs},
		{":memory:", ""},
		{"file::memory:?cache=shared", ""},
		{"file:" + slash + "?mode=memory", ""},
	} {
		if got := sqliteFile(tc.dsn); got != tc.want {
			t.Errorf("sqliteFile(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
}

// sync-hooks skips a repository whose directory is missing, rather than
// leave a hooks-only directory the server takes for the repository, and a
// repository whose hooks cannot be written does not stop the others; the
// errors are returned together (R47, CFG-8).
func TestInitializeHooksSkipsMissingAndContinues(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	c := &cobra.Command{}
	var logs bytes.Buffer
	ctx := log.WithContext(config.WithContext(context.Background(), cfg), log.New(&logs))
	c.SetContext(ctx)
	if err := InitBackendContext(c, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseDBContext(c, nil) })
	ctx = c.Context()
	if err := migrate.Migrate(ctx, db.FromContext(ctx)); err != nil {
		t.Fatal(err)
	}
	be := backend.FromContext(ctx)
	owner, err := be.DefaultAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-broken", "b-gone", "c-ok"} {
		if _, err := be.CreateSystemRepository(ctx, name, owner, proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	repo := func(name string) string { return filepath.Join(cfg.DataPath, "repos", name+".git") }
	// a-broken's hooks directory is a file, so its hooks cannot be written.
	if err := os.RemoveAll(filepath.Join(repo("a-broken"), "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo("a-broken"), "hooks"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo("b-gone")); err != nil {
		t.Fatal(err)
	}
	okHook := filepath.Join(repo("c-ok"), "hooks", "update")
	if err := os.Remove(okHook); err != nil {
		t.Fatal(err)
	}

	err = InitializeHooks(ctx, cfg, be)
	if err == nil || !strings.Contains(err.Error(), "a-broken") || strings.Contains(err.Error(), "c-ok") {
		t.Errorf("InitializeHooks() = %v, want only a-broken's error", err)
	}
	if _, err := os.Stat(repo("b-gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("b-gone's directory was created: %v", err)
	}
	if !strings.Contains(logs.String(), "b-gone") {
		t.Errorf("no warning about b-gone: %q", logs.String())
	}
	if _, err := os.Stat(okHook); err != nil {
		t.Errorf("c-ok's hooks were not written after a-broken failed: %v", err)
	}
}
