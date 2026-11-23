package ui

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/selection"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // sqlite driver
)

// The subtests run one at a time: building the UI pages writes the shared
// style config.
func TestUIErrorScreenHidesInternalErrors(t *testing.T) {
	const leak = "/var/lib/soft-serve/repos/secret.git"
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string
		logged  bool
	}{
		{
			name:    "internal error",
			err:     errors.New("open repository: detected dubious ownership in repository at '" + leak + "'"),
			want:    "something went wrong",
			notWant: leak,
			logged:  true,
		},
		{
			name: "user error",
			err:  common.ErrMissingRepo,
			want: "missing repo",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := backendtest.New(t)
			uc, err := common.New(t.Context(), e.Cfg, e.Backend, log.New(&buf))
			if err != nil {
				t.Fatal(err)
			}
			ui := New(uc, 120, 40, "")
			t.Cleanup(ui.common.Zone.Close)
			ui.Init()

			ui.Update(common.ErrorMsg{Err: c.err})
			view := ui.View().Content
			if !strings.Contains(view, c.want) {
				t.Errorf("error screen does not show %q:\n%s", c.want, view)
			}
			if c.notWant != "" && strings.Contains(view, c.notWant) {
				t.Errorf("error screen shows %q:\n%s", c.notWant, view)
			}
			if got := strings.Contains(buf.String(), c.err.Error()); got != c.logged {
				t.Errorf("error logged = %v, want %v; log:\n%s", got, c.logged, buf.String())
			}
		})
	}
}

// filteringPage is a selection page stand-in whose filter is active.
type filteringPage struct{ keys []string }

func (p *filteringPage) Init() tea.Cmd { return nil }
func (p *filteringPage) Update(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		p.keys = append(p.keys, k.String())
	}
	return nil
}
func (p *filteringPage) View() string              { return "" }
func (p *filteringPage) SetSize(int, int)          {}
func (p *filteringPage) ShortHelp() []key.Binding  { return nil }
func (p *filteringPage) FullHelp() [][]key.Binding { return nil }
func (p *filteringPage) IsFiltering() bool         { return true }

func newFilteringUI(t *testing.T) (*Model, *filteringPage) {
	t.Helper()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	page := &filteringPage{}
	ui := &Model{
		common:     c,
		width:      80,
		height:     24,
		pages:      []common.Component{page, page},
		activePage: selectionPage,
		state:      readyState,
		serverName: "test",
		showFooter: true,
	}
	ui.footer = footer.New(c, ui)
	return ui, page
}

func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, runCmd(c)...)
		}
		return msgs
	}
	return []tea.Msg{msg}
}

// While the repository list is being filtered, "q" and "?" are typed into the
// filter, but ctrl+c still quits.
func TestUIKeysWhileFiltering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		key      tea.KeyPressMsg
		wantQuit bool
	}{
		{name: "q", key: tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{name: "?", key: tea.KeyPressMsg{Code: '?', Text: "?"}},
		{name: "ctrl+c", key: tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, wantQuit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ui, page := newFilteringUI(t)
			_, cmd := ui.Update(tc.key)
			var quit, toggled bool
			for _, msg := range runCmd(cmd) {
				switch msg.(type) {
				case tea.QuitMsg:
					quit = true
				case footer.ToggleFooterMsg:
					toggled = true
				}
			}
			if quit != tc.wantQuit {
				t.Errorf("quit = %v, want %v", quit, tc.wantQuit)
			}
			if toggled {
				t.Error("help was toggled while filtering")
			}
			if !tc.wantQuit && (len(page.keys) != 1 || page.keys[0] != tc.name) {
				t.Errorf("page got keys %q, want [%q]", page.keys, tc.name)
			}
		})
	}
}

// recordingPage is a page that records the messages it receives.
type recordingPage struct {
	msgs []tea.Msg
}

func (p *recordingPage) Init() tea.Cmd { return nil }

func (p *recordingPage) Update(msg tea.Msg) tea.Cmd {
	p.msgs = append(p.msgs, msg)
	return nil
}

func (p *recordingPage) View() string              { return "" }
func (p *recordingPage) SetSize(int, int)          {}
func (p *recordingPage) ShortHelp() []key.Binding  { return nil }
func (p *recordingPage) FullHelp() [][]key.Binding { return nil }
func (p *recordingPage) inputs() (n int) {
	for _, m := range p.msgs {
		if common.IsInputMsg(m) {
			n++
		}
	}
	return n
}

func newTestUI(t *testing.T, active page) (*Model, *recordingPage) {
	t.Helper()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	p := &recordingPage{}
	ui := &Model{
		common:     c,
		width:      80,
		height:     24,
		pages:      []common.Component{&recordingPage{}, &recordingPage{}},
		activePage: active,
		state:      readyState,
		serverName: "test",
		showFooter: true,
	}
	ui.pages[active] = p
	ui.footer = footer.New(c, ui)
	return ui, p
}

var (
	escKey  = tea.KeyPressMsg{Code: tea.KeyEscape}
	downKey = tea.KeyPressMsg{Code: tea.KeyDown}
)

func TestUIErrorStateKeepsInputFromPage(t *testing.T) {
	t.Parallel()
	ui, p := newTestUI(t, selectionPage)
	ui.Update(common.ErrorMsg{Err: errors.New("binary file")})

	ui.Update(downKey)
	ui.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	ui.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if n := p.inputs(); n != 0 {
		t.Fatalf("hidden page received %d input messages while the error was shown", n)
	}

	// The key that dismisses the error must not also act on the page.
	ui.Update(escKey)
	if ui.state != readyState || ui.error != nil {
		t.Fatalf("esc did not dismiss the error: state=%v error=%v", ui.state, ui.error)
	}
	if n := p.inputs(); n != 0 {
		t.Fatalf("dismissing esc reached the page (%d input messages)", n)
	}

	// Once dismissed, input reaches the page again.
	ui.Update(downKey)
	if n := p.inputs(); n != 1 {
		t.Fatalf("page received %d input messages after the error was dismissed, want 1", n)
	}
}

func TestUIErrorStateForwardsNonInput(t *testing.T) {
	t.Parallel()
	ui, p := newTestUI(t, selectionPage)
	ui.Update(common.ErrorMsg{Err: errors.New("boom")})
	before := len(p.msgs)
	type otherMsg struct{}
	ui.Update(otherMsg{})
	if len(p.msgs) != before+1 {
		t.Fatal("non-input message was not forwarded to the page while the error was shown")
	}
}

func TestUIDismissErrorKeepsSelectionFooter(t *testing.T) {
	t.Parallel()
	ui, _ := newTestUI(t, selectionPage)
	if ui.footer.ShowAll() {
		t.Fatal("footer unexpectedly starts with all help shown")
	}
	ui.Update(common.ErrorMsg{Err: common.ErrMissingRepo})
	ui.Update(escKey)
	if !ui.showFooter {
		t.Fatal("footer hidden on the selection page after dismissing an error")
	}
}

func TestUIDismissErrorOnRepoPageFollowsShowAll(t *testing.T) {
	t.Parallel()
	ui, _ := newTestUI(t, repoPage)
	ui.Update(common.ErrorMsg{Err: errors.New("binary file")})
	ui.Update(escKey)
	if ui.showFooter {
		t.Fatal("footer shown on the repo page after dismissing an error without show-all help")
	}
}

// resizeCountingPage is a page that counts the window size messages it gets.
type resizeCountingPage struct{ resizes int }

func (p *resizeCountingPage) Init() tea.Cmd { return nil }
func (p *resizeCountingPage) Update(msg tea.Msg) tea.Cmd {
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		p.resizes++
	}
	return nil
}
func (p *resizeCountingPage) View() string              { return "" }
func (p *resizeCountingPage) ShortHelp() []key.Binding  { return nil }
func (p *resizeCountingPage) FullHelp() [][]key.Binding { return nil }
func (p *resizeCountingPage) SetSize(int, int)          {}

// A resize reaches every page once, the active page included.
func TestUIDeliversResizeOnce(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	ui := New(c, 80, 40, "")
	sel, rp := &resizeCountingPage{}, &resizeCountingPage{}
	ui.pages = []common.Component{sel, rp}
	ui.state = readyState

	for _, active := range []page{selectionPage, repoPage} {
		ui.activePage = active
		sel.resizes, rp.resizes = 0, 0
		ui.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
		if sel.resizes != 1 || rp.resizes != 1 {
			t.Errorf("page %d active: selection got %d resizes, repo got %d, want 1 each",
				active, sel.resizes, rp.resizes)
		}
	}
}

// renderWait bounds how long a test waits for a render to finish.
const renderWait = time.Minute

// TestUIQuitKeepsZoneManagerRunning checks that quitting leaves the zone
// manager running for the program's last render. A render after the manager
// is closed blocks forever once the view has more zones than its worker
// buffers, which a tall terminal on a long list reaches.
func TestUIQuitKeepsZoneManagerRunning(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	ui := New(c, 80, 24, "")

	_, cmd := ui.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("quit key returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("quit key did not quit")
	}

	// The last render scans every zone of the view.
	var view strings.Builder
	for i := range 500 {
		view.WriteString(c.Zone.Mark(fmt.Sprintf("zone-%d", i), "x"))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Zone.Scan(view.String())
	}()
	select {
	case <-done:
	case <-time.After(renderWait):
		t.Fatal("render after quit blocked: the zone manager was closed on quit")
	}
}

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

// errorLike is a message that happens to have an Error method.
type errorLike struct{}

func (errorLike) Error() string { return "not an error report" }

// Only common.ErrorMsg shows the error screen, not any message with an Error
// method.
func TestUIErrorScreenOnlyForErrorMsg(t *testing.T) {
	t.Parallel()
	ui, p := newTestUI(t, selectionPage)
	ui.Update(errorLike{})
	if ui.state == errorState || ui.error != nil {
		t.Fatal("a message with an Error method showed the error screen")
	}
	if len(p.msgs) != 1 {
		t.Fatalf("page got %d messages; want the message passed on", len(p.msgs))
	}
}

// Toggling the help footer on the repository page resizes the page before
// the page handles the toggle, so the Commits tab loads its page once, by the
// height it has with the footer, rather than by the old height and then
// again.
func TestUIFooterToggleLoadsCommitPageOnce(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)
	commitTo(t, ui, "a")

	// send delivers msg and every message the commands produce, and returns
	// how many commit pages were delivered.
	send := func(msg tea.Msg) int {
		t.Helper()
		loads := 0
		queue := []tea.Msg{msg}
		for steps := 0; len(queue) > 0; steps++ {
			if steps > 10000 {
				t.Fatal("UI never settles")
			}
			msg, queue = queue[0], queue[1:]
			if _, ok := msg.(spinner.TickMsg); ok || msg == nil {
				continue
			}
			if _, ok := msg.(repo.LogItemsMsg); ok {
				loads++
			}
			_, cmd := ui.Update(msg)
			queue = append(queue, runCmd(cmd)...)
		}
		return loads
	}

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	send(ui.openRepoCmd("a")())
	send(tabs.SelectTabMsg(2))

	help := tea.KeyPressMsg{Code: '?', Text: "?"}
	for i, state := range []string{"shown", "hidden"} {
		if n := send(help); n != 1 {
			t.Fatalf("toggle %d, help %s: the commit page was loaded %d times; want once", i+1, state, n)
		}
	}
}

// The selection page starts with the server name, trimmed.
func TestUISelectionShowsServerName(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Name = "  Test Server  "
	ui := New(uitest.NewCommon(t, t.Context(), cfg, nil), 80, 24, "")
	ui.Init()

	var first string
	for _, line := range strings.Split(ansi.Strip(ui.View().Content), "\n") {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	// The style pads the name by one cell on each side and indents it by
	// one more, inside the app's two-cell margin.
	if want := "    Test Server "; strings.TrimRight(first, " ") != strings.TrimRight(want, " ") {
		t.Fatalf("first line = %q; want the server name %q", first, want)
	}
}

func uiTestKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// openRepo uses the user AuthenticationMiddleware stored on the session
// context instead of resolving the key again each time a repository is
// opened, and an admin key on the context still opens every repository.
func TestOpenRepoUsesSessionUser(t *testing.T) {
	t.Parallel()
	open := func(ui *Model, rn string) (proto.Repository, error) {
		msg, ok := ui.openRepoCmd(rn)().(repoOpenedMsg)
		if !ok {
			t.Fatal("openRepoCmd did not return a repoOpenedMsg")
		}
		return msg.repo, msg.err
	}

	adminKey := uiTestKey(t)
	e := backendtest.New(t, backendtest.WithConfig(func(cfg *config.Config) {
		cfg.InitialAdminKeys = []string{proto.MarshalAuthorizedKey(adminKey)}
	}))
	ctx, cfg, be := t.Context(), e.Cfg, e.Backend

	pk := uiTestKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []gossh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	// The session authenticated as owner; the key then stops resolving
	// to the account, so only the session user grants access.
	sessCtx := proto.WithUserContext(context.WithValue(ctx, ssh.ContextKeyPublicKey, pk), owner)
	if err := be.RemovePublicKey(ctx, "owner", pk); err != nil {
		t.Fatal(err)
	}
	if _, err := be.UserByPublicKey(ctx, pk); !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatalf("the key still resolves (%v); the test proves nothing", err)
	}

	ui := New(uitest.NewCommon(t, sessCtx, cfg, be), 80, 40, "")
	r, err := open(ui, "private")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Name(); got != "private" {
		t.Fatalf("r.Name() = %v, want %v", got, "private")
	}

	// An admin key no account owns opens the private repository too.
	adminCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, adminKey)
	ui = New(uitest.NewCommon(t, adminCtx, cfg, be), 80, 40, "")
	r, err = open(ui, "private")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Name(); got != "private" {
		t.Fatalf("r.Name() = %v, want %v", got, "private")
	}

	// Anyone else gets the missing-repository error.
	otherCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, uiTestKey(t))
	ui = New(uitest.NewCommon(t, otherCtx, cfg, be), 80, 40, "")
	_, err = open(ui, "private")
	if got, want := err, common.ErrMissingRepo; !reflect.DeepEqual(got, want) {
		t.Fatalf("err = %v, want %v", got, want)
	}
}

// commitTo pushes a commit that adds a file to the server repository name.
func commitTo(t *testing.T, ui *Model, name string) {
	t.Helper()
	r, err := ui.common.Backend.Repository(ui.common.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	gr, err := r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("diff-body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "the-subject")
	run("push", "-q", gr.Path, "main")
}

// settleUI delivers msgs to ui and every message their commands produce,
// until none is left, dropping spinner ticks.
func settleUI(t *testing.T, ui *Model, msgs ...tea.Msg) {
	t.Helper()
	for steps := 0; len(msgs) > 0; steps++ {
		if steps > 10000 {
			t.Fatal("UI never settles")
		}
		msg := msgs[0]
		msgs = msgs[1:]
		if _, ok := msg.(spinner.TickMsg); ok {
			continue
		}
		_, cmd := ui.Update(msg)
		msgs = append(msgs, runCmd(cmd)...)
	}
}

// openCommit opens repository "a" on its Commits tab and opens its commit.
func openCommit(t *testing.T, ui *Model) {
	t.Helper()
	settleUI(t, ui, ui.openRepoCmd("a")())
	settleUI(t, ui, tabs.SelectTabMsg(2))
	settleUI(t, ui, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// A diff render still running when the user leaves the repository page
// returns while the selection page is shown. It must reach the Commits tab,
// or the tab waits for it for ever and shows no diff again.
func TestUIDiffRenderAfterLeavingRepository(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)
	commitTo(t, ui, "a")
	settleUI(t, ui, tea.WindowSizeMsg{Width: 80, Height: 40})
	openCommit(t, ui)
	if v := ansi.Strip(ui.View().Content); !strings.Contains(v, "diff-body") {
		t.Fatalf("the diff is not shown:\n%s", v)
	}

	// A width change starts a render; the user leaves before it returns.
	_, cmd := ui.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	var late []tea.Msg
	for _, m := range runCmd(cmd) {
		if _, ok := m.(repo.RenderedMsg); ok {
			late = append(late, m)
		}
	}
	if len(late) != 1 {
		t.Fatalf("the width change started %d renders; want 1", len(late))
	}
	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	settleUI(t, ui, esc)
	settleUI(t, ui, esc)
	if ui.activePage != selectionPage {
		t.Fatal("esc did not leave the repository page")
	}
	// The UI hands the render to the repository page, which is not shown.
	// This check is the part of the test that tests the routing. The end
	// to end half below passes even when the render is lost: opening a
	// commit resets the tab's render gate, so the diff opened again renders
	// whether or not the late render arrived.
	routing, shown := newTestUI(t, selectionPage)
	routing.Update(late[0])
	if got := routing.pages[repoPage].(*recordingPage).msgs; len(got) != 1 || len(shown.msgs) != 0 {
		t.Fatalf("the render reached the repository page %d times and the selection page %d times; want 1 and 0",
			len(got), len(shown.msgs))
	}
	settleUI(t, ui, late...)

	openCommit(t, ui)
	v := ansi.Strip(ui.View().Content)
	if strings.Contains(v, "loading commit") || !strings.Contains(v, "diff-body") {
		t.Fatalf("the diff opened after leaving the page is not shown:\n%s", v)
	}
}

// A code view renders in a command, and its result goes to every page: the
// server README on the selection page renders while the repository page is
// shown.
func TestUIDeliversRendersToEveryPage(t *testing.T) {
	t.Parallel()
	ui, active := newTestUI(t, repoPage)
	inactive := ui.pages[selectionPage].(*recordingPage)
	ui.Update(code.RenderedMsg{})
	if len(inactive.msgs) != 1 || len(active.msgs) != 1 {
		t.Fatalf("render delivered %d times to the selection page and %d times to the repository page; want once each",
			len(inactive.msgs), len(active.msgs))
	}
}

// newRepoTestUI returns a UI backed by a real backend that holds the
// repositories "a" and "b". The pages are built without Init, which would load
// the repository list.
func newRepoTestUI(t *testing.T) *Model {
	t.Helper()
	ctx := t.Context()
	e := backendtest.New(t)
	cfg, be := e.Cfg, e.Backend
	for _, name := range []string{"a", "b"} {
		if _, err := be.CreateRepository(ctx, name, fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	ui := New(uitest.NewCommon(t, ctx, cfg, be), 80, 40, "")
	ui.pages[selectionPage] = selection.New(ui.common)
	ui.pages[repoPage] = repo.New(ui.common,
		repo.NewReadme(ui.common),
		repo.NewFiles(ui.common),
		repo.NewLog(ui.common),
		repo.NewRefs(ui.common, git.RefsHeads),
		repo.NewRefs(ui.common, git.RefsTags),
	)
	ui.state = readyState
	return ui
}

// shownRepo returns the name of the repository the UI shows, or "" on the
// selection page.
func shownRepo(t *testing.T, ui *Model) string {
	t.Helper()
	if ui.activePage != repoPage {
		return ""
	}
	rp, ok := ui.pages[repoPage].(*repo.Repo)
	if !ok {
		t.Fatalf("repository page is %T", ui.pages[repoPage])
	}
	r := rp.Repository()
	if r == nil {
		t.Fatal("repository page shown without a repository")
	}
	return r.Name()
}

// TestUIDropsLateRepoOpen opens "a" and then "b", and delivers the result for
// "a" last. The UI must keep showing "b".
func TestUIDropsLateRepoOpen(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)

	openA := ui.openRepoCmd("a")
	openB := ui.openRepoCmd("b")
	msgB := openB()
	msgA := openA()

	ui.Update(msgB)
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("after opening b the UI shows %q", got)
	}
	ui.Update(msgA)
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("a late open of a replaced b: the UI shows %q", got)
	}
}

// TestUIDropsLateRepoOpenError checks that a failed open the user has moved on
// from does not show the error screen over the current repository.
func TestUIDropsLateRepoOpenError(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)

	openMissing := ui.openRepoCmd("missing")
	openB := ui.openRepoCmd("b")
	ui.Update(openB())
	ui.Update(openMissing())
	if ui.state == errorState {
		t.Fatalf("a late error was shown: %v", ui.error)
	}
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("the UI shows %q, want b", got)
	}

	// The newest open still reports its error.
	ui.Update(ui.openRepoCmd("missing")())
	if ui.state != errorState || !errors.Is(ui.error, common.ErrMissingRepo) {
		t.Fatalf("state %v, error %v; want the missing repository error", ui.state, ui.error)
	}
}

// TestUIInitialRepoOpen checks the initial repository: it is shown when it
// opens, a missing one shows the missing repository error, and one
// that finishes after the user picked another repository is dropped.
func TestUIInitialRepoOpen(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)
	ui.Update(ui.openRepoCmd("a")())
	if got := shownRepo(t, ui); got != "a" {
		t.Fatalf("initial repository: the UI shows %q, want a", got)
	}

	ui = newRepoTestUI(t)
	ui.Update(ui.openRepoCmd("missing")())
	if ui.state != errorState || !errors.Is(ui.error, common.ErrMissingRepo) {
		t.Fatalf("a missing initial repository: state %v, error %v; want the missing repository error", ui.state, ui.error)
	}

	ui = newRepoTestUI(t)
	initial := ui.openRepoCmd("a")
	ui.Update(ui.openRepoCmd("b")())
	ui.Update(initial())
	if got := shownRepo(t, ui); got != "b" {
		t.Fatalf("a late initial repository replaced b: the UI shows %q", got)
	}
}

// TestUIRepoOpenDoesNotReadModel runs an open command while Update shows
// another repository, which replaces the UI's context. The command must work
// on the values copied when it was built; under -race, reading the UI from
// the command goroutine is reported as a data race.
func TestUIRepoOpenDoesNotReadModel(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)
	rb, err := ui.common.Backend.Repository(ui.common.Context(), "b")
	if err != nil {
		t.Fatal(err)
	}

	for range 20 {
		open := ui.openRepoCmd("a")
		var wg sync.WaitGroup
		var msg any
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg = open()
		}()
		ui.Update(repo.OpenedMsg(rb))
		wg.Wait()
		m, ok := msg.(repoOpenedMsg)
		if !ok || m.err != nil || m.repo == nil || m.repo.Name() != "a" {
			t.Fatalf("open returned %#v, want repository a", msg)
		}
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
