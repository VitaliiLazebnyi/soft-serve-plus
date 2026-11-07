package ssh

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// resizeCountingPage is a page that counts the window size messages it gets.
type resizeCountingPage struct{ resizes int }

func (p *resizeCountingPage) Init() tea.Cmd { return nil }
func (p *resizeCountingPage) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		p.resizes++
	}
	return p, nil
}
func (p *resizeCountingPage) View() string              { return "" }
func (p *resizeCountingPage) ShortHelp() []key.Binding  { return nil }
func (p *resizeCountingPage) FullHelp() [][]key.Binding { return nil }
func (p *resizeCountingPage) SetSize(int, int)          {}

// A resize reaches every page once, the active page included.
func TestUIDeliversResizeOnce(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	ui := NewUI(common.NewCommon(ctx, 80, 40), "")
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
