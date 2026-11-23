package selection

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// A resize reaches the README view once, whichever tab is active.
func TestSelectionDeliversResizeOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(uitest.NewCommon(t, ctx, nil, nil))
	s.SetSize(80, 40)
	for _, p := range []pane{selectorPane, readmePane} {
		s.activePane = p
		before := s.readme.Resizes()
		s.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
		if got := s.readme.Resizes() - before; got != 1 {
			t.Errorf("with %s active: README view got the resize %d times, want 1", p, got)
		}
	}
}
