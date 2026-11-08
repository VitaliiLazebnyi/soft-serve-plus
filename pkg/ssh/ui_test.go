package ssh

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/header"
)

// newTestUIContext returns a context holding a config, store and backend
// backed by a fresh database, for building a UI.
func newTestUIContext(t *testing.T) context.Context {
	t.Helper()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	ctx := config.WithContext(context.Background(), cfg)
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
	return backend.WithContext(ctx, backend.New(ctx, cfg, dbx, dbstore))
}

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
			ctx := log.WithContext(newTestUIContext(t), log.New(&buf))
			ui := NewUI(common.NewCommon(ctx, 120, 40), "")
			t.Cleanup(ui.common.Zone.Close)
			ui.Init()

			ui.Update(common.ErrorMsg(c.err))
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

// recordingPage is a page that records the messages it receives.
type recordingPage struct {
	msgs []tea.Msg
}

func (p *recordingPage) Init() tea.Cmd { return nil }

func (p *recordingPage) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	p.msgs = append(p.msgs, msg)
	return p, nil
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

func newTestUI(t *testing.T, active page) (*UI, *recordingPage) {
	t.Helper()
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	p := &recordingPage{}
	ui := &UI{
		common:     c,
		pages:      []common.Component{&recordingPage{}, &recordingPage{}},
		activePage: active,
		state:      readyState,
		header:     header.New(c, "test"),
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
	ui.Update(common.ErrorMsg(errors.New("binary file")))

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
	ui.Update(common.ErrorMsg(errors.New("boom")))
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
	ui.Update(common.ErrorMsg(common.ErrMissingRepo))
	ui.Update(escKey)
	if !ui.showFooter {
		t.Fatal("footer hidden on the selection page after dismissing an error")
	}
}

func TestUIDismissErrorOnRepoPageFollowsShowAll(t *testing.T) {
	t.Parallel()
	ui, _ := newTestUI(t, repoPage)
	ui.Update(common.ErrorMsg(errors.New("binary file")))
	ui.Update(escKey)
	if ui.showFooter {
		t.Fatal("footer shown on the repo page after dismissing an error without show-all help")
	}
}
