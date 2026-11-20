package admin

import (
	"context"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	_ "modernc.org/sqlite" // sqlite driver
)

// When an admin command fails, it still closes the database: cobra skips
// the command's PersistentPostRunE, which closes it otherwise.
func TestAdminErrorClosesDB(t *testing.T) {
	// Run twice, each with its own data path. Regression: the admin command
	// tree was package-level, and cobra keeps a subcommand's context from
	// its first run, so a second run used the first run's config and data
	// path.
	for _, name := range []string{"first", "second"} {
		t.Run(name, testAdminErrorClosesDB)
	}
}

func testAdminErrorClosesDB(t *testing.T) {
	dataPath := t.TempDir()
	t.Setenv("SOFT_SERVE_DATA_PATH", dataPath)
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	// A fresh database has no migrations to roll back.
	command := NewCommand()
	command.SetArgs([]string{"rollback", "--yes"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	rollback, err := command.ExecuteContextC(ctx)
	if err == nil || !strings.Contains(err.Error(), "rollback:") {
		t.Fatalf("admin rollback returned %v, want a rollback error", err)
	}

	if got := config.FromContext(rollback.Context()).DataPath; got != dataPath {
		t.Errorf("rollback ran with data path %s, want this run's %s", got, dataPath)
	}
	dbx := db.FromContext(rollback.Context())
	if dbx == nil {
		t.Fatal("no database in the command context")
	}
	if err := dbx.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("database ping returned %v, want it closed", err)
	}
}

func newAdminContext(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	return log.WithContext(ctx, log.New(io.Discard))
}

// "soft admin rollback" undoes a migration, which may drop data, so it asks
// first and does nothing unless the answer is yes.
func TestAdminRollbackAsksForConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"no input", "", errRollbackNotConfirmed.Error()},
		{"declined", "n\n", errRollbackNotConfirmed.Error()},
		// A fresh database has no migrations to roll back, so a confirmed
		// rollback gets as far as that error.
		{"confirmed", "yes\n", "rollback: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newAdminContext(t)
			var stderr strings.Builder
			command := NewCommand()
			command.SetArgs([]string{"rollback"})
			command.SetIn(strings.NewReader(tc.input))
			command.SetOut(io.Discard)
			command.SetErr(&stderr)
			err := command.ExecuteContext(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admin rollback returned %v, want %q", err, tc.want)
			}
			if !strings.Contains(stderr.String(), "[y/N]") {
				t.Errorf("admin rollback did not ask for confirmation: %q", stderr.String())
			}
		})
	}
}

// An unknown admin subcommand is an error rather than the help and a
// successful exit.
func TestAdminRejectsUnknownSubcommand(t *testing.T) {
	ctx := newAdminContext(t)
	command := NewCommand()
	command.SetArgs([]string{"rollbak"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	err := command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("admin rollbak returned %v, want an unknown command error", err)
	}
}
