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

	// Each message is built for the generation that is current when it is
	// sent, since the repository page drops messages from an older one.
	for _, tc := range []struct {
		msg  func(gen uint64) tea.Msg
		want map[*countingPane]int
	}{
		{func(uint64) tea.Msg { return tea.WindowSizeMsg{Width: 100, Height: 50} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(uint64) tea.Msg { return RepoMsg(fakeRepo{}) }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return RefMsg{gen: g} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return EmptyRepoMsg{gen: g} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return ReadmeMsg{gen: g} }, map[*countingPane]int{readme: 1}},
		{func(g uint64) tea.Msg { return FileContentMsg{gen: g} }, map[*countingPane]int{files: 1}},
		{func(g uint64) tea.Msg { return LogCountMsg{count: 1, gen: g} }, map[*countingPane]int{log: 1}},
		{func(uint64) tea.Msg { return spinner.TickMsg{ID: -2} }, map[*countingPane]int{}},
	} {
		// Each tab takes a turn as the active one, so that the tab a message is
		// addressed to is active as well as inactive.
		for a := range r.panes {
			r.activeTab = a
			msg := tc.msg(r.gen)
			before := map[*countingPane]int{}
			for _, p := range []*countingPane{readme, files, log} {
				before[p] = p.count(msg)
			}
			r.Update(msg)
			for _, p := range []*countingPane{readme, files, log} {
				if got := p.count(msg) - before[p]; got != tc.want[p] {
					t.Errorf("%T with tab %d active: pane %q got it %d times, want %d",
						msg, a, p.name, got, tc.want[p])
				}
			}
		}
	}
}

// A resize reaches the code view of the Files tab once while a file is open.
func TestFilesDeliversResizeToContentOnce(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.activeView = filesViewContent
	f.currentContent = FileContentMsg{content: "hello", ext: ".txt"}
	f.code.SetContent("hello", ".txt")

	before := f.code.Resizes()
	f.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	if got := f.code.Resizes() - before; got != 1 {
		t.Errorf("code view got the resize %d times, want 1", got)
	}
}
