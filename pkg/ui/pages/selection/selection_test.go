package selection

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // sqlite driver
)

// testServer is a backendtest.Env's config, database and backend.
type testServer struct {
	cfg *config.Config
	db  *db.DB
	be  *backend.Backend
}

// newTestBackend returns a backendtest.Env as a testServer.
func newTestBackend(t *testing.T) *testServer {
	t.Helper()
	e := backendtest.New(t)
	return &testServer{cfg: e.Cfg, db: e.DB, be: e.Backend}
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
	msg, ok := uitest.Receive(ch)
	if !ok {
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

// cursorPkg is the package of the cursor blink messages.
var cursorPkg = reflect.TypeOf(cursor.BlinkMsg{}).PkgPath()

// filterDriver feeds keys to a selection page and the messages their
// commands produce back to it, the way the bubbletea runtime does. It
// collects the SelectMsg the page sends, which the UI above it handles.
type filterDriver struct {
	t       *testing.T
	s       *Selection
	selects []selector.SelectMsg
}

// run runs cmd to completion, however slow the machine, and returns its
// messages. The test turns off the filter input's cursor blinking, whose
// every blink waits half a second; the cursor messages left are dropped
// instead of delivered, as delivering one could schedule another.
func (d *filterDriver) run(cmd tea.Cmd) []tea.Msg {
	d.t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	msg, ok := uitest.Receive(ch)
	if !ok {
		d.t.Fatal("command did not return")
	}
	if msg != nil && reflect.TypeOf(msg).PkgPath() == cursorPkg {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, d.run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// noBlink turns off the blinking of the filter input's cursor, so that a
// test does not wait half a second for each blink.
func noBlink(s *Selection) {
	s.selector.SetFilterCursorBlink(false)
}

func (d *filterDriver) send(msg tea.Msg) {
	d.t.Helper()
	queue := []tea.Msg{msg}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			d.t.Fatal("page never settles")
		}
		msg, queue = queue[0], queue[1:]
		if sel, ok := msg.(selector.SelectMsg); ok {
			d.selects = append(d.selects, sel)
		}
		queue = append(queue, d.run(d.s.Update(msg))...)
	}
}

func (d *filterDriver) want(step string, state list.FilterState, selected string, selects int) {
	d.t.Helper()
	if got := d.s.selector.FilterState(); got != state {
		d.t.Fatalf("%s: filter state = %v; want %v", step, got, state)
	}
	if it := d.s.selector.SelectedItem(); it == nil || it.ID() != selected {
		d.t.Fatalf("%s: highlighted %v; want %q", step, it, selected)
	}
	if len(d.selects) != selects {
		d.t.Fatalf("%s: %d SelectMsg; want %d", step, len(d.selects), selects)
	}
}

// Filtering the repository list: "/" starts the filter, typed keys go to it,
// the first enter applies it, the second opens the highlighted repository,
// and esc clears the filter.
func TestSelectionFilterFlow(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	s := New(c)
	s.SetSize(80, 40)
	its := make([]selector.IdentifiableItem, 0, 3)
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		its = append(its, NewItem(c, fakeRepo{name: n}))
	}
	s.setItems(its)
	noBlink(s)
	d := &filterDriver{t: t, s: s}
	key := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

	d.want("start", list.Unfiltered, "alpha", 0)

	d.send(key('/'))
	d.want("after /", list.Filtering, "alpha", 0)
	if !s.IsFiltering() {
		t.Fatal("page does not report filtering")
	}

	d.send(key('b'))
	d.send(key('r'))
	d.want("after typing", list.Filtering, "bravo", 0)
	if n := len(s.selector.VisibleItems()); n != 1 {
		t.Fatalf("%d repositories match %q; want 1", n, "br")
	}

	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	d.send(enter)
	d.want("after the first enter", list.FilterApplied, "bravo", 0)

	d.send(enter)
	d.want("after the second enter", list.FilterApplied, "bravo", 1)
	if got := d.selects[0].ID(); got != "bravo" {
		t.Fatalf("SelectMsg for %q; want bravo", got)
	}

	d.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := s.selector.FilterState(); got != list.Unfiltered {
		t.Fatalf("after esc: filter state = %v; want unfiltered", got)
	}
	if n := len(s.selector.VisibleItems()); n != 3 {
		t.Fatalf("after esc: %d repositories listed; want 3", n)
	}
	if len(d.selects) != 1 {
		t.Fatalf("after esc: %d SelectMsg; want 1", len(d.selects))
	}
}

// The About tab says "loading…" instead of its scroll position while the
// readme renders, since what it shows meanwhile is not that readme.
func TestSelectionAboutStatusWhileRendering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	s.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	s.activePane = readmePane
	if v := s.View(); strings.Contains(v, "loading…") || !strings.Contains(v, "≡") {
		t.Fatalf("with no readme: the status is not the scroll position:\n%s", v)
	}

	cmd := s.setReadme("# Banner", "README.md")
	if cmd == nil {
		t.Fatal("setting the readme started no render")
	}
	if v := s.View(); !strings.Contains(v, "loading…") || strings.Contains(v, "≡") {
		t.Fatalf("while the readme renders: the status does not say loading:\n%s", v)
	}

	msg, ok := cmd().(code.RenderedMsg)
	if !ok {
		t.Fatal("the render returned no RenderedMsg")
	}
	s.Update(msg)
	if v := s.View(); strings.Contains(v, "loading…") || !strings.Contains(v, "≡") || !strings.Contains(v, "Banner") {
		t.Fatalf("once the readme rendered: it or its scroll position is missing:\n%s", v)
	}
}

// A refresh cancels the listing in flight, which then lists nothing.
func TestRefreshCancelsListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	first := s.Init()
	s.Init()
	ran := func() (ran bool) {
		// The test has no backend, so a listing that runs panics.
		defer func() {
			if recover() != nil {
				ran = true
			}
		}()
		return first() != nil
	}()
	if ran {
		t.Fatal("a superseded listing ran")
	}
}

// A resize reaches the README view once, whichever tab is active.
func TestSelectionDeliversResizeOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	for _, p := range []pane{selectorPane, readmePane} {
		s.activePane = p
		before := s.readme.Resizes()
		s.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
		if got := s.readme.Resizes() - before; got != 1 {
			t.Errorf("with %s active: README view got the resize %d times, want 1", p, got)
		}
	}
}

// escapes holds the string sequences the bubbletea renderer passes through
// to the terminal: a DCS, an APC, an OSC 8 hyperlink with a hidden target, an
// OSC 52 clipboard write and a C1 CSI.
const escapes = "\x1bP1|x\x1b\\" +
	"\x1b_Gx\x1b\\" +
	"\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" +
	"\x1b]52;c;Zm9v\x07" +
	"\u009b2J"

// csi matches the CSI sequences the TUI itself emits (SGR styles and zone
// marks); anything else that controls the terminal must come from data.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// assertClean fails if out, less the TUI's own CSI sequences, holds a
// control character other than newline and tab, or lacks any of want.
func assertClean(t *testing.T, out string, want ...string) {
	t.Helper()
	rest := csi.ReplaceAllString(out, "")
	for _, r := range rest {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("output holds control character %q: %q", r, out)
		}
	}
	for _, w := range want {
		if !strings.Contains(rest, w) {
			t.Errorf("output lacks %q: %q", w, rest)
		}
	}
}

// A project name and description any repository creator controls are shown
// to everyone on the repository list, so they must not carry escape
// sequences into the viewer's terminal.
func TestItemSanitisesProjectNameAndDescription(t *testing.T) {
	t.Parallel()
	out := renderItem(t, fakeRepo{
		name:        "repo",
		projectName: "proj" + escapes,
		description: "desc" + escapes,
	}, 80)
	assertClean(t, out, "proj", "desc")
}

// The server README is shown to everyone who connects, and glamour keeps
// string sequences in it.
func TestReadmePaneSanitisesReadme(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	s := New(c)
	s.SetSize(120, 40)
	s.Update(s.setReadme("# Welcome"+escapes+"\n\nreadme-text"+escapes+"\n", "README.md")())
	s.activePane = readmePane
	assertClean(t, s.View(), "Welcome", "readme-text")
}

// The list shows the repositories of the session's user, as the session
// resolved it. A lookup of the session's key that fails does not turn the
// list into what an anonymous caller sees.
func TestSelectionListsSessionUserRepos(t *testing.T) {
	t.Parallel()
	srv := newTestBackend(t)
	ctx, be := t.Context(), srv.be
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk := signer.PublicKey()
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "mine", owner, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatal(err)
	}

	// Key lookups now fail; the session's user was resolved before.
	if _, err := srv.db.ExecContext(ctx, "ALTER TABLE public_keys RENAME TO public_keys_gone"); err != nil {
		t.Fatal(err)
	}

	sessCtx := context.WithValue(proto.WithUserContext(ctx, owner), ssh.ContextKeyPublicKey, pk)
	c := srv.common(t, sessCtx)
	s := New(c)
	s.SetSize(80, 40)
	if !runLoad(t, s, s.Init()) {
		t.Fatal("init did not load the repositories")
	}
	if got := listedNames(s); !slices.Contains(got, "mine") {
		t.Fatalf("listed %v, want the session user's private repository", got)
	}
}

// A keyless session whose keyless setting cannot be read gets the error
// screen, not an empty list as if there were nothing to show.
func TestSelectionKeylessSettingLookupFailure(t *testing.T) {
	t.Parallel()
	srv := newTestBackend(t)
	ctx := t.Context()
	if _, err := srv.db.ExecContext(ctx, "ALTER TABLE settings RENAME TO settings_gone"); err != nil {
		t.Fatal(err)
	}
	c := srv.common(t, ctx)
	msg, ok := loadRepos(ctx, c).(common.ErrorMsg)
	if !ok || msg.Err == nil {
		t.Fatalf("loadRepos = %#v, want an error message", msg)
	}
}
