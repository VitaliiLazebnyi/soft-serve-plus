package repo

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// The mouse wheel scrolls the commit list.
func TestLogMouseWheel(t *testing.T) {
	t.Parallel()
	l := NewLog(common.NewCommon(context.Background(), 80, 24))
	l.selector.SetItems([]selector.IdentifiableItem{
		LogItem{&git.Commit{Message: "one"}},
		LogItem{&git.Commit{Message: "two"}},
	})

	m, _ := l.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	l = m.(*Log)
	if got := l.selector.Index(); got != 1 {
		t.Fatalf("index after wheel down = %d; want 1", got)
	}
}
