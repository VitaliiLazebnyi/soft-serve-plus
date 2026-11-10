package selector

import (
	"context"
	"io"
	"slices"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type nopDelegate struct{}

func (nopDelegate) Render(io.Writer, list.Model, int, list.Item) {}
func (nopDelegate) Height() int                                  { return 1 }
func (nopDelegate) Spacing() int                                 { return 0 }
func (nopDelegate) Update(tea.Msg, *list.Model) tea.Cmd          { return nil }

// Every repository pane list hides the list chrome, cannot be filtered or
// quit from, and pages with the shared page keys.
func TestNewPane(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	s := NewPane(c, nopDelegate{})
	m := s.Model
	if m.ShowTitle() || m.ShowFilter() || m.ShowHelp() || m.ShowPagination() || m.ShowStatusBar() {
		t.Error("pane list shows list chrome")
	}
	if m.FilteringEnabled() {
		t.Error("pane list can be filtered")
	}
	if m.KeyMap.Quit.Enabled() || m.KeyMap.ForceQuit.Enabled() {
		t.Error("pane list has quit keys")
	}
	if !slices.Equal(m.KeyMap.NextPage.Keys(), c.KeyMap.NextPage.Keys()) {
		t.Errorf("next page keys = %v, want %v", m.KeyMap.NextPage.Keys(), c.KeyMap.NextPage.Keys())
	}
	if !slices.Equal(m.KeyMap.PrevPage.Keys(), c.KeyMap.PrevPage.Keys()) {
		t.Errorf("prev page keys = %v, want %v", m.KeyMap.PrevPage.Keys(), c.KeyMap.PrevPage.Keys())
	}
}
