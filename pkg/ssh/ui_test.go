package ssh

import (
	"context"
	"errors"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/header"
)

// filteringPage is a selection page stand-in whose filter is active.
type filteringPage struct{ keys []string }

func (p *filteringPage) Init() tea.Cmd { return nil }
func (p *filteringPage) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		p.keys = append(p.keys, k.String())
	}
	return p, nil
}
func (p *filteringPage) View() string              { return "" }
func (p *filteringPage) SetSize(int, int)          {}
func (p *filteringPage) ShortHelp() []key.Binding  { return nil }
func (p *filteringPage) FullHelp() [][]key.Binding { return nil }
func (p *filteringPage) IsFiltering() bool         { return true }

func newFilteringUI(t *testing.T) (*UI, *filteringPage) {
	t.Helper()
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	page := &filteringPage{}
	ui := &UI{
		common:     c,
		pages:      []common.Component{page, page},
		activePage: selectionPage,
		state:      readyState,
		header:     header.New(c, "test"),
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
