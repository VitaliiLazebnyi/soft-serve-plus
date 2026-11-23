package backendtest

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// The config names the test database: a second handle opened from it sees
// the rows the backend wrote through the first.
func TestNewConfigNamesTheDatabase(t *testing.T) {
	t.Parallel()
	e := New(t, WithConfig(func(cfg *config.Config) { cfg.Name = "fixture" }))
	if e.Cfg.Name != "fixture" {
		t.Errorf("Cfg.Name = %q, want %q", e.Cfg.Name, "fixture")
	}
	if _, err := e.Backend.CreateUser(e.Ctx, "alice", proto.UserOptions{}); err != nil {
		t.Fatal(err)
	}
	other, err := db.Open(e.Ctx, e.Cfg.DB.Driver, e.Cfg.DB.DataSource, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	var n int
	if err := other.GetContext(e.Ctx, &n, other.Rebind(`SELECT COUNT(*) FROM users WHERE username = ?`), "alice"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users named alice through the config's data source = %d, want 1", n)
	}
}
