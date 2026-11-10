package code

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

// A resize re-renders the content only when the width changes.
func TestCodeRerendersOnlyOnWidthChange(t *testing.T) {
	c := New(common.NewCommon(context.Background(), 80, 20), "", "")
	c.SetContent("before", ".txt")

	// Swap the content behind the renderer's back, so that the view shows
	// whether a resize rendered it again.
	c.content = "after"

	c.SetSize(80, 30)
	c.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if view := ansi.Strip(c.View()); !strings.Contains(view, "before") {
		t.Fatalf("a height-only resize rendered the content again:\n%s", view)
	}

	c.SetSize(100, 30)
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if view := ansi.Strip(c.View()); !strings.Contains(view, "after") {
		t.Fatalf("a width change did not render the content again:\n%s", view)
	}
}
