package ssh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/header"
)

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

// TestUIQuitKeepsZoneManagerRunning checks that quitting leaves the zone
// manager running for the program's last render. A render after the manager
// is closed blocks forever once the view has more zones than its worker
// buffers, which a tall terminal on a long list reaches.
func TestUIQuitKeepsZoneManagerRunning(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 24)
	t.Cleanup(c.Zone.Close)
	ui := NewUI(c, "")

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
	case <-time.After(10 * time.Second):
		t.Fatal("render after quit blocked: the zone manager was closed on quit")
	}
}
