package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
)

// A repository lookup that fails is a server fault: the daemon answers with
// its fixed malfunction message and logs the cause, rather than answering
// as for a missing or private repository. The test has its own database,
// since it breaks it.
func TestAuthorizeLookupFailure(t *testing.T) {
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	ctx := log.WithContext(config.WithContext(context.Background(), cfg), log.New(logs))
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	st := database.New()
	ctx = store.WithContext(db.WithContext(ctx, dbx), st)
	be := backend.New(ctx, cfg, dbx, st)
	ctx = backend.WithContext(ctx, be)
	if _, err := be.CreateRepository(ctx, "public", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	req := gitRequest{service: git.UploadPackService, name: "public"}
	if err := d.authorize(ctx, req); err != nil {
		t.Fatalf("authorize = %v, want nil", err)
	}

	if _, err := dbx.ExecContext(ctx, "ALTER TABLE repos RENAME TO repos_gone"); err != nil {
		t.Fatal(err)
	}
	if err := d.authorize(ctx, req); !errors.Is(err, git.ErrSystemMalfunction) {
		t.Fatalf("authorize with the lookup failing = %v, want %v", err, git.ErrSystemMalfunction)
	}
	if !strings.Contains(logs.String(), "failed to look up repository") {
		t.Errorf("logs = %q, want the failed lookup", logs.String())
	}
}
