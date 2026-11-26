package backend

import (
	"context"
	"errors"
	"io"
	"testing"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

// newTestBackend returns a Backend on a freshly migrated test database
// from dbtest (Postgres when SOFT_SERVE_DB_DRIVER is "postgres", SQLite
// otherwise), along with the *config.Config it was constructed with (so
// tests can mutate AnonAccess/AllowKeyless directly to simulate config
// overrides). AdminOnlyCreate is left at its default (off); tests of the
// restricted policy turn it on themselves.
//
// The packages outside pkg/backend use internal/backendtest, which this
// package's tests cannot import.
func newTestBackend(t *testing.T) (*Backend, *config.Config) {
	t.Helper()
	return newTestBackendWithLog(t, nil)
}

// newTestBackendWithLog is newTestBackend with the backend's logger writing to
// logs, so a test can assert on what the backend logged. A nil logs keeps the
// default logger.
func newTestBackendWithLog(t *testing.T, logs io.Writer) (*Backend, *config.Config) {
	t.Helper()
	return newTestBackendWithQuery(t, logs, "")
}

// newTestBackendWithQuery is newTestBackendWithLog, on a SQLite database
// whose data source has query, which starts with "&", added, such as to set
// its journal mode, when query is not empty.
func newTestBackendWithQuery(t *testing.T, logs io.Writer, query string) (*Backend, *config.Config) {
	t.Helper()
	var logger *charmlog.Logger
	if logs != nil {
		logger = charmlog.New(logs)
	}
	return openTestBackend(t, logger, query)
}

// openTestBackend is newTestBackendWithQuery with the backend logging to
// logger; nil is the default logger.
func openTestBackend(t *testing.T, logger *charmlog.Logger, query string) (*Backend, *config.Config) {
	t.Helper()
	ctx := context.Background()

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	var dbx *db.DB
	var err error
	if query == "" {
		dbx, cfg.DB.DataSource, err = dbtest.OpenSource(ctx, t)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		cfg.DB.DataSource = cfg.DataPath + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" + query
		dbx, err = db.Open(ctx, "sqlite", cfg.DB.DataSource, db.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	}
	cfg.DB.Driver = dbx.DriverName()

	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	return New(cfg, dbx, database.New(nil), logger), cfg
}

// levelOf is the access level Readable gives user's account on repo.
func levelOf(t *testing.T, be *Backend, ctx context.Context, repo string, user proto.User) access.Level {
	t.Helper()
	_, level, err := be.Readable(ctx, repo, user)
	if err != nil {
		t.Fatal(err)
	}
	return level
}

// baseLevel is BaseAccessLevelForUser, failing the test if the lookup failed.
func baseLevel(t *testing.T, be *Backend, ctx context.Context, user proto.User) access.Level {
	t.Helper()
	level, err := be.BaseAccessLevelForUser(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	return level
}

// noLookupError takes the results of a Readable or ReadableForUser call and
// returns the repository and level, failing the test if the lookup failed:
// noLookupError(t)(be.Readable(ctx, name, user)).
func noLookupError(t *testing.T) func(proto.Repository, access.Level, error) (proto.Repository, access.Level) {
	return func(r proto.Repository, level access.Level, err error) (proto.Repository, access.Level) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r, level
	}
}

// collabOf is IsCollaborator for the repository and user with these names.
// A missing repository has no collaborators.
func collabOf(t *testing.T, be *Backend, ctx context.Context, repo, username string) (access.Level, bool, error) {
	t.Helper()
	u, err := be.User(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	r, err := be.Repository(ctx, repo)
	if errors.Is(err, proto.ErrRepoNotFound) {
		return be.IsCollaborator(ctx, nil, u)
	}
	if err != nil {
		t.Fatal(err)
	}
	return be.IsCollaborator(ctx, r, u)
}

// publicKeysOf returns the public keys of the user named username.
func publicKeysOf(be *Backend, ctx context.Context, username string) ([]ssh.PublicKey, error) {
	u, err := be.User(ctx, username)
	if err != nil {
		return nil, err
	}
	return u.PublicKeys(), nil
}

// setAdminKeys lists keys as initial_admin_keys and validates cfg, which is
// where the config parses them, as a restart with the new list would.
func setAdminKeys(t *testing.T, cfg *config.Config, keys []string) {
	t.Helper()
	cfg.InitialAdminKeys = keys
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAnonAccessConfigOverride(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	// Seeded default from migration, no override set.
	if got := be.AnonAccess(ctx); got != access.ReadOnlyAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.ReadOnlyAccess)
	}

	// With no override, DB writes via SetAnonAccess are reflected.
	if err := be.SetAnonAccess(ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if got := be.AnonAccess(ctx); got != access.ReadWriteAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.ReadWriteAccess)
	}

	// A config override takes precedence over whatever is in the DB.
	admin := access.AdminAccess
	cfg.AnonAccess = &admin
	if got := be.AnonAccess(ctx); got != access.AdminAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.AdminAccess)
	}

	// The DB value is unchanged underneath the override: once the override
	// is cleared, the last DB write is what's returned again.
	cfg.AnonAccess = nil
	if got := be.AnonAccess(ctx); got != access.ReadWriteAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.ReadWriteAccess)
	}
}

func TestAllowKeylessConfigOverride(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	// Seeded default from migration, no override set.
	if got := be.AllowKeyless(ctx); got != true {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, true)
	}

	// With no override, DB writes via SetAllowKeyless are reflected.
	if err := be.SetAllowKeyless(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := be.AllowKeyless(ctx); got != false {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, false)
	}

	// A config override of true takes precedence over a DB value of false.
	allow := true
	cfg.AllowKeyless = &allow
	if got := be.AllowKeyless(ctx); got != true {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, true)
	}

	// A config override of false takes precedence over a DB value of true
	// too — this is a real tri-state override, not just a way to force
	// things open.
	if err := be.SetAllowKeyless(ctx, true); err != nil {
		t.Fatal(err)
	}
	disallow := false
	cfg.AllowKeyless = &disallow
	if got := be.AllowKeyless(ctx); got != false {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, false)
	}

	// Clearing the override falls back to the DB value again.
	cfg.AllowKeyless = nil
	if got := be.AllowKeyless(ctx); got != true {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, true)
	}
}
