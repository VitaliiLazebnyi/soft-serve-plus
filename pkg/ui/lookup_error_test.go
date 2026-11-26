package ui

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// During an outage the TUI opens a missing name and an existing private one
// alike: neither is reported as a missing repository, so the error screen
// does not tell which names exist.
func TestOpenRepoOutageSameAnswer(t *testing.T) {
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			e := backendtest.New(t)
			ctx, dbx, be := e.Ctx, e.DB, e.Backend
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
