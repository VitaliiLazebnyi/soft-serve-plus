package selection

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	_ "modernc.org/sqlite" // sqlite driver
)

// testServer is a migrated SQLite backend, its config and database.
type testServer struct {
	cfg *config.Config
	db  *db.DB
	be  *backend.Backend
}

// newTestBackend returns a migrated SQLite backend.
func newTestBackend(t *testing.T) *testServer {
	t.Helper()
	ctx := t.Context()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	be := backend.New(cfg, dbx, database.New(), log.Default())
	return &testServer{cfg: cfg, db: dbx, be: be}
}

// common returns the Common of a session on the server whose context is
// ctx.
func (srv *testServer) common(t *testing.T, ctx context.Context) *common.Common {
	t.Helper()
	return uitest.NewCommon(t, ctx, srv.cfg, srv.be)
}

// newTestCommon returns the Common of a keyless session on a server that
// holds one public repository.
func newTestCommon(t *testing.T) *common.Common {
	t.Helper()
	srv := newTestBackend(t)
	if _, err := srv.be.CreateRepository(t.Context(), "a", fixtureAdmin(t, srv.be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	return srv.common(t, t.Context())
}

// bubbletea runs the command Init returns on its own goroutine while Update
// resizes the page after every message. Loading the repositories must work
// on what Init captured and not read the model, which raced with SetSize.
func TestLoadReposCmdDoesNotReadModel(t *testing.T) {
	t.Parallel()
	c := newTestCommon(t)
	s := New(c)
	s.SetSize(80, 40)

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

func listedNames(s *Selection) []string {
	var names []string
	for _, it := range s.selector.Items() {
		names = append(names, it.(Item).ID())
	}
	return names
}

// runLoad runs cmd and the commands it batches to completion, however slow
// the machine, and feeds every ReposMsg they return back to Update, as
// bubbletea would. Other messages, timers such as a cursor blink included,
// are dropped by their type. It reports whether a ReposMsg was delivered.
func runLoad(t *testing.T, s *Selection, cmd tea.Cmd) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(uitest.HangGuard):
		t.Fatal("command did not return")
	}
	switch msg := msg.(type) {
	case tea.BatchMsg:
		loaded := false
		for _, c := range msg {
			if runLoad(t, s, c) {
				loaded = true
			}
		}
		return loaded
	case ReposMsg:
		s.Update(msg)
		return true
	}
	return false
}

func press(s *Selection, k rune) tea.Cmd {
	cmd := s.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
	return cmd
}

// The refresh key reloads the repository list, so a repository created
// after the page opened shows up, and the highlighted repository stays
// highlighted.
//
// Init and the refresh key load the list in a command, so the tests run it
// and feed its ReposMsg back to Update before checking the list.
func TestSelectionRefreshKey(t *testing.T) {
	t.Parallel()
	srv := newTestBackend(t)
	ctx, be := t.Context(), srv.be
	for _, name := range []string{"bravo", "delta"} {
		if _, err := be.CreateRepository(ctx, name, fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	c := srv.common(t, ctx)
	s := New(c)
	s.SetSize(80, 40)
	if !runLoad(t, s, s.Init()) {
		t.Fatal("init did not load the repositories")
	}
	if got := listedNames(s); len(got) != 2 {
		t.Fatalf("listed %v, want bravo and delta", got)
	}
	s.selector.Select(slices.Index(listedNames(s), "delta"))
	if got := s.selector.SelectedItem().ID(); got != "delta" {
		t.Fatalf("highlighted %q, want delta", got)
	}

	if _, err := be.CreateRepository(ctx, "alpha", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := listedNames(s); len(got) != 2 {
		t.Fatalf("listed %v before refresh, want the list unchanged", got)
	}

	if !runLoad(t, s, press(s, 'r')) {
		t.Fatal("r did not reload the repositories")
	}
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
	t.Parallel()
	srv := newTestBackend(t)
	ctx, be := t.Context(), srv.be
	if _, err := be.CreateRepository(ctx, "bravo", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	c := srv.common(t, ctx)
	s := New(c)
	s.SetSize(80, 40)
	if !runLoad(t, s, s.Init()) {
		t.Fatal("init did not load the repositories")
	}
	noBlink(s)

	press(s, '/')
	if !s.IsFiltering() {
		t.Fatal("/ did not start filtering")
	}
	if _, err := be.CreateRepository(ctx, "alpha", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if runLoad(t, s, press(s, 'r')) {
		t.Error("r reloaded the repositories while filtering")
	}
	if got := listedNames(s); len(got) != 1 {
		t.Errorf("listed %v, want r typed into the filter, not a refresh", got)
	}
}

// Replacing the items keeps the highlighted repository highlighted even
// when its position in the list changes.
func TestSelectionSetItemsKeepsHighlight(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	s := New(c)
	s.SetSize(80, 40)
	items := func(names ...string) []selector.IdentifiableItem {
		its := make([]selector.IdentifiableItem, len(names))
		for i, n := range names {
			its[i] = NewItem(c, fakeRepo{name: n})
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
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	s := New(c)
	s.SetSize(80, 40)
	it := NewItem(c, fakeRepo{name: "bravo"})
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

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}
