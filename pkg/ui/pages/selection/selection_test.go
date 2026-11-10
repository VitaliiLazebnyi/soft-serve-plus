package selection

import (
	"context"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	_ "modernc.org/sqlite" // sqlite driver
)

// newTestContext returns a context carrying a config and a backend backed by
// a fresh sqlite database that holds one public repository.
func newTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	ctx = backend.WithContext(ctx, be)
	if _, err := be.CreateRepository(ctx, "a", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// bubbletea runs the command Init returns on its own goroutine while Update
// resizes the page after every message. Loading the repositories must work
// on what Init captured and not read the model, which raced with SetSize.
func TestLoadReposCmdDoesNotReadModel(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(newTestContext(t), 80, 40)
	t.Cleanup(c.Zone.Close)
	s := New(c)

	cmd := s.Init()
	if cmd == nil {
		t.Fatal("init returned no command")
	}

	var (
		wg  sync.WaitGroup
		msg tea.Msg
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		msg = cmd()
	}()
	for i := range 100 {
		s.SetSize(80+i, 40+i)
	}
	wg.Wait()

	repos, ok := msg.(ReposMsg)
	if !ok {
		t.Fatalf("init command returned %#v, want a ReposMsg", msg)
	}
	if len(repos.items) != 1 || repos.items[0].ID() != "a" {
		t.Fatalf("listed items = %#v, want repository a", repos.items)
	}
}
