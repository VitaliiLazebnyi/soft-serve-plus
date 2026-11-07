package ssh

import (
	"context"
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
