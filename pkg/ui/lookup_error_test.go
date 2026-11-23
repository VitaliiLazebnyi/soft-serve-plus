package ui

import (
	"context"
	"errors"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// During an outage the TUI opens a missing name and an existing private one
// alike: neither is reported as a missing repository, so the error screen
// does not tell which names exist.
func TestOpenRepoOutageSameAnswer(t *testing.T) {
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			dbx, err := dbtest.Open(ctx, t)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			cfg.DB.Driver = dbx.DriverName()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			st := database.New(nil)
			be := backend.New(cfg, dbx, st, log.Default())
			owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true}); err != nil {
				t.Fatal(err)
			}
			stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}

			if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
				t.Fatal(err)
			}
			for _, user := range []proto.User{nil, stranger} {
				for _, name := range []string{"private", "ghost"} {
					_, err := openRepo(ctx, be, user, name)
					if err == nil || errors.Is(err, common.ErrMissingRepo) {
						t.Fatalf("openRepo(%s) = %v, want a failed lookup", name, err)
					}
				}
			}
		})
	}
}
