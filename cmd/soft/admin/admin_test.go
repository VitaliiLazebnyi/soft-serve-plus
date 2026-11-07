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
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	// A fresh database has no migrations to roll back.
	t.Cleanup(func() { rollbackYes = false })
	Command.SetArgs([]string{"rollback", "--yes"})
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	err := Command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "rollback:") {
		t.Fatalf("admin rollback returned %v, want a rollback error", err)
	}

	dbx := db.FromContext(rollbackCmd.Context())
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
			Command.SetArgs([]string{"rollback"})
			Command.SetIn(strings.NewReader(tc.input))
			Command.SetOut(io.Discard)
			Command.SetErr(&stderr)
			err := Command.ExecuteContext(ctx)
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
	Command.SetArgs([]string{"rollbak"})
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	err := Command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("admin rollbak returned %v, want an unknown command error", err)
	}
}
