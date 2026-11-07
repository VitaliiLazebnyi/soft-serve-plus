package selection

import (
	"context"
	"slices"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	_ "modernc.org/sqlite"
)

// newTestBackend returns a context carrying a config and a migrated SQLite
// backend, with no user or key attached.
func newTestBackend(t *testing.T) (context.Context, *backend.Backend) {
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
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	ctx = db.WithContext(ctx, dbx)
	st := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, st)
	be := backend.New(ctx, cfg, dbx, st)
	ctx = backend.WithContext(ctx, be)
	return ctx, be
}

func listedNames(s *Selection) []string {
	var names []string
	for _, it := range s.selector.Items() {
		names = append(names, it.(Item).ID())
	}
	return names
}

func press(s *Selection, k rune) tea.Cmd {
	_, cmd := s.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
	return cmd
}

// The refresh key reloads the repository list, so a repository created
// after the page opened shows up, and the highlighted repository stays
// highlighted.
//
// These tests rely on Init and the refresh key loading the list synchronously.
// If the load moves into a tea.Cmd (as the listing fan-out change does with
// loadReposCmd/ReposMsg), the highlight-keeping setItems call moves into the
// ReposMsg handler, and the tests must run the returned command and feed its
// ReposMsg back to Update before checking the list.
func TestSelectionRefreshKey(t *testing.T) {
	ctx, be := newTestBackend(t)
	for _, name := range []string{"bravo", "delta"} {
		if _, err := be.CreateRepository(ctx, name, nil, proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	c := common.NewCommon(ctx, 80, 40)
	s := New(c)
	s.SetSize(80, 40)
	s.Init()
	if got := listedNames(s); len(got) != 2 {
		t.Fatalf("listed %v, want bravo and delta", got)
	}
	s.selector.Select(slices.Index(listedNames(s), "delta"))
	if got := s.selector.SelectedItem().ID(); got != "delta" {
		t.Fatalf("highlighted %q, want delta", got)
	}

	if _, err := be.CreateRepository(ctx, "alpha", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := listedNames(s); len(got) != 2 {
		t.Fatalf("listed %v before refresh, want the list unchanged", got)
	}

	press(s, 'r')
	got := listedNames(s)
	if len(got) != 3 || !slices.Contains(got, "alpha") {
		t.Fatalf("listed %v after refresh, want alpha, bravo and delta", got)
	}
	if id := s.selector.SelectedItem().ID(); id != "delta" {
		t.Errorf("highlighted %q after refresh, want delta to stay highlighted", id)
	}
}

// While the filter is being typed, r is part of the filter text and must not
// reload the list.
func TestSelectionRefreshKeyIgnoredWhileFiltering(t *testing.T) {
	ctx, be := newTestBackend(t)
	if _, err := be.CreateRepository(ctx, "bravo", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	c := common.NewCommon(ctx, 80, 40)
	s := New(c)
	s.SetSize(80, 40)
	s.Init()

	press(s, '/')
	if !s.IsFiltering() {
		t.Fatal("/ did not start filtering")
	}
	if _, err := be.CreateRepository(ctx, "alpha", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	press(s, 'r')
	if got := listedNames(s); len(got) != 1 {
		t.Errorf("listed %v, want r typed into the filter, not a refresh", got)
	}
}

// Replacing the items keeps the highlighted repository highlighted even
// when its position in the list changes.
func TestSelectionSetItemsKeepsHighlight(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 40)
	s := New(c)
	s.SetSize(80, 40)
	items := func(names ...string) []selector.IdentifiableItem {
		its := make([]selector.IdentifiableItem, len(names))
		for i, n := range names {
			it, err := NewItem(c, fakeRepo{name: n})
			if err != nil {
				t.Fatal(err)
			}
			its[i] = it
		}
		return its
	}

	s.setItems(items("a", "b", "c"))
	s.selector.Select(2)
	s.setItems(items("x", "y", "a", "b", "c"))
	if got := s.selector.SelectedItem().ID(); got != "c" {
		t.Errorf("highlighted %q after new items were added before it, want c", got)
	}
	s.setItems(items("c", "z"))
	if got := s.selector.SelectedItem().ID(); got != "c" {
		t.Errorf("highlighted %q after items before it were removed, want c", got)
	}
	s.setItems(items("p", "q"))
	if got := s.selector.SelectedItem(); got == nil {
		t.Error("nothing highlighted after the highlighted repository went away")
	}
}

func hasBinding(kb []key.Binding, want key.Binding) bool {
	for _, b := range kb {
		if slices.Equal(b.Keys(), want.Keys()) && b.Help() == want.Help() {
			return true
		}
	}
	return false
}

// The refresh key is listed in the short help of the repository list, and
// not while the filter is typed, when r goes into the filter text.
func TestSelectionShortHelpRefreshKey(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 40)
	s := New(c)
	s.SetSize(80, 40)
	it, err := NewItem(c, fakeRepo{name: "bravo"})
	if err != nil {
		t.Fatal(err)
	}
	s.setItems([]selector.IdentifiableItem{it})
	if !hasBinding(s.ShortHelp(), refreshKey) {
		t.Error("short help does not list the refresh key")
	}
	press(s, '/')
	if !s.IsFiltering() {
		t.Fatal("/ did not start filtering")
	}
	if hasBinding(s.ShortHelp(), refreshKey) {
		t.Error("short help lists the refresh key while filtering")
	}
}
