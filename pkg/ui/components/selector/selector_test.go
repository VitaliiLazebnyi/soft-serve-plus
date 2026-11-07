package selector

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type testItem string

func (i testItem) ID() string          { return string(i) }
func (i testItem) Title() string       { return string(i) }
func (i testItem) Description() string { return "" }
func (i testItem) FilterValue() string { return string(i) }

// The mouse wheel moves the cursor: bubbletea v2 reports it as a
// MouseWheelMsg, never as a click.
func TestMouseWheelMovesCursor(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	s := New(c, []IdentifiableItem{testItem("a"), testItem("b"), testItem("c")}, list.NewDefaultDelegate())

	for _, step := range []struct {
		button tea.MouseButton
		want   int
	}{
		{tea.MouseWheelDown, 1},
		{tea.MouseWheelDown, 2},
		{tea.MouseWheelUp, 1},
	} {
		m, _ := s.Update(tea.MouseWheelMsg{Button: step.button})
		s = m.(*Selector)
		if got := s.Index(); got != step.want {
			t.Fatalf("after %v: index = %d; want %d", step.button, got, step.want)
		}
	}
}
