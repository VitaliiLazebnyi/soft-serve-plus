package repo

import (
	"context"
	"fmt"
	"testing"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// countingPane is a tab that counts the messages it receives by type.
type countingPane struct {
	name string
	got  map[string]int
}

func newCountingPane(name string) *countingPane {
	return &countingPane{name: name, got: map[string]int{}}
}

func (p *countingPane) Init() tea.Cmd { return nil }
func (p *countingPane) Update(msg tea.Msg) (common.Model, tea.Cmd) {
	p.got[fmt.Sprintf("%T", msg)]++
	return p, nil
}
func (p *countingPane) View() string              { return "" }
func (p *countingPane) ShortHelp() []key.Binding  { return nil }
func (p *countingPane) FullHelp() [][]key.Binding { return nil }
func (p *countingPane) SetSize(int, int)          {}
func (p *countingPane) StatusBarValue() string    { return "" }
func (p *countingPane) StatusBarInfo() string     { return "" }
func (p *countingPane) SpinnerID() int            { return -1 }
func (p *countingPane) TabName() string           { return p.name }
func (p *countingPane) Path() string              { return "" }
func (p *countingPane) count(msg tea.Msg) int     { return p.got[fmt.Sprintf("%T", msg)] }

var _ common.TabComponent = (*countingPane)(nil)

// Every message reaches each pane it is meant for exactly once, the active
// pane included.
func TestRepoDeliversMessagesOnce(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	readme := newCountingPane((&Readme{}).TabName())
	files := newCountingPane((&Files{}).TabName())
	log := newCountingPane((&Log{}).TabName())
	r := New(c, readme, files, log)

	for _, tc := range []struct {
		msg  tea.Msg
		want map[*countingPane]int
	}{
		{tea.WindowSizeMsg{Width: 100, Height: 50}, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{RepoMsg(fakeRepo{}), map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{EmptyRepoMsg{}, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{ReadmeMsg{}, map[*countingPane]int{readme: 1}},
		{FileContentMsg{}, map[*countingPane]int{files: 1}},
		{LogCountMsg(1), map[*countingPane]int{log: 1}},
		{spinner.TickMsg{ID: -2}, map[*countingPane]int{}},
	} {
		// Each tab takes a turn as the active one, so that the tab a message is
		// addressed to is active as well as inactive.
		for a := range r.panes {
			r.activeTab = a
			before := map[*countingPane]int{}
			for _, p := range []*countingPane{readme, files, log} {
				before[p] = p.count(tc.msg)
			}
			r.Update(tc.msg)
			for _, p := range []*countingPane{readme, files, log} {
				if got := p.count(tc.msg) - before[p]; got != tc.want[p] {
					t.Errorf("%T with tab %d active: pane %q got it %d times, want %d",
						tc.msg, a, p.name, got, tc.want[p])
				}
			}
		}
	}
}
