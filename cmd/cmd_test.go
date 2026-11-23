package cmd

import (
	"bytes"
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
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// WithEnv closes the Env it opened, the database and the log, whether its
// run fails or not, and returns run's error.
func TestWithEnvClosesEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runErr error
	}{
		{name: "error", runErr: errors.New("boom")},
		{name: "success", runErr: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, t.TempDir())
			logClosed := false
			load := func() (*config.Config, *log.Logger, func() error, error) {
				return cfg, log.New(io.Discard), func() error { logClosed = true; return nil }, nil
			}
			var dbx *db.DB
			run := WithEnv(load, true, func(_ *cobra.Command, _ []string, env *Env) error {
				dbx = env.DB
				return tc.runErr
			})
			c := &cobra.Command{}
			c.SetContext(t.Context())
			if err := run(c, nil); !errors.Is(err, tc.runErr) {
				t.Fatalf("run returned %v, want %v", err, tc.runErr)
			}
			err := dbx.PingContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), "closed") {
				t.Errorf("database ping returned %v, want it closed", err)
			}
			if !logClosed {
				t.Error("the log was left open")
			}
		})
	}
}

// OpenEnv applies the db pool settings from the config to the database it
// opens.
func TestOpenEnvAppliesPoolLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.MaxOpenConns = 7
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	env, err := OpenEnv(t.Context(), Given(cfg, nil), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Close() })
	if got := env.DB.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}
}

// OpenEnv for serve creates a missing data directory that only the
// server's user can open, since it holds private repositories and the
// database.
func TestOpenEnvCreatesPrivateDataPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "data")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	env, err := OpenEnv(t.Context(), Given(cfg, nil), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Close() })
	fi, err := os.Stat(cfg.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("data path mode = %v, want %v", got, os.FileMode(0o700))
	}
}

// Given gives its config and logger, the default logger for a nil one, and
// refuses a nil config.
func TestGiven(t *testing.T) {
	cfg := config.DefaultConfig()
	logger := log.New(io.Discard)
	got, gotLogger, closeLog, err := Given(cfg, logger)()
	if err != nil || got != cfg || gotLogger != logger || closeLog != nil {
		t.Errorf("Given(cfg, logger)() = %p, %p, %v, %v; want cfg and logger", got, gotLogger, closeLog != nil, err)
	}
	if _, gotLogger, _, _ := Given(cfg, nil)(); gotLogger != log.Default() {
		t.Error("Given(cfg, nil) did not give the default logger")
	}
	if _, _, _, err := Given(nil, logger)(); !errors.Is(err, config.ErrNilConfig) {
		t.Errorf("Given(nil, logger)() = %v, want %v", err, config.ErrNilConfig)
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
func TestOpenEnvRefusesMissingDataPath(t *testing.T) {
	dp := filepath.Join(t.TempDir(), "data")
	_, err := OpenEnv(t.Context(), Given(testConfig(t, dp), nil), false)
	if err == nil || !strings.Contains(err.Error(), "SOFT_SERVE_DATA_PATH") || !strings.Contains(err.Error(), dp) {
		t.Fatalf("OpenEnv() = %v, want an error naming the data directory and SOFT_SERVE_DATA_PATH", err)
	}
	if _, err := os.Stat(dp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing data directory was created: %v", err)
	}
}

func TestOpenEnvRefusesMissingDatabase(t *testing.T) {
	dp := t.TempDir()
	cfg := testConfig(t, dp)
	file := filepath.Join(dp, "soft-serve.db")
	_, err := OpenEnv(t.Context(), Given(cfg, nil), false)
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("OpenEnv() = %v, want an error naming %s", err, file)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing database was created: %v", err)
	}

	// Once serve has created it, a maintenance command opens it.
	env, err := OpenEnv(t.Context(), Given(cfg, nil), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Close(); err != nil {
		t.Fatal(err)
	}
	env, err = OpenEnv(t.Context(), Given(cfg, nil), false)
	if err != nil {
		t.Fatalf("OpenEnv() on an existing database = %v", err)
	}
	if err := env.Close(); err != nil {
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
	var logs bytes.Buffer
	logger := log.New(&logs)
	ctx := t.Context()
	env, err := OpenEnv(ctx, Given(cfg, logger), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Close() })
	if err := migrate.Migrate(ctx, env.DB, cfg, nil); err != nil {
		t.Fatal(err)
	}
	be := env.Backend
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

	err = InitializeHooks(ctx, cfg, be, logger)
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

// With Postgres there is no database file to look for: a data directory
// that exists is enough, whatever the data source names. Checking the data
// source as a file would refuse every Postgres setup.
func TestCheckDataPathPostgres(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = "postgres://soft@127.0.0.1:5432/soft?sslmode=disable"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := checkDataPath(cfg); err != nil {
		t.Errorf("checkDataPath() with Postgres and an existing data directory = %v, want nil", err)
	}

	// The data directory is still required.
	cfg.DataPath = filepath.Join(cfg.DataPath, "missing")
	if err := checkDataPath(cfg); err == nil || !strings.Contains(err.Error(), "SOFT_SERVE_DATA_PATH") {
		t.Errorf("checkDataPath() with Postgres and no data directory = %v, want the data directory error", err)
	}
}
