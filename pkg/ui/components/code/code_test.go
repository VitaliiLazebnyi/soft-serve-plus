package code

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// A resize re-renders the content only when the width changes.
func TestCodeRerendersOnlyOnWidthChange(t *testing.T) {
	t.Parallel()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(80, 20)
	deliver(c, c.Update(tea.WindowSizeMsg{Width: 80, Height: 20}))
	deliver(c, c.SetContent("before", ".txt"))

	// Swap the content behind the renderer's back, so that the view shows
	// whether a resize rendered it again.
	c.content = "after"

	c.SetSize(80, 30)
	deliver(c, c.Update(tea.WindowSizeMsg{Width: 80, Height: 30}))
	if view := ansi.Strip(c.View()); !strings.Contains(view, "before") {
		t.Fatalf("a height-only resize rendered the content again:\n%s", view)
	}

	c.SetSize(100, 30)
	deliver(c, c.Update(tea.WindowSizeMsg{Width: 100, Height: 30}))
	if view := ansi.Strip(c.View()); !strings.Contains(view, "after") {
		t.Fatalf("a width change did not render the content again:\n%s", view)
	}
}

// deliver runs cmd and hands the renders it produces back to c, as the
// program would.
func deliver(c *Code, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, cmd := range msg {
			deliver(c, cmd)
		}
	case RenderedMsg:
		deliver(c, c.Update(msg))
	}
}

// A line wider than the view wraps; only its first part is numbered, and no
// line is padded to the width.
func TestCodeNumbersWrappedLines(t *testing.T) {
	t.Parallel()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(40, 20)
	c.ShowLineNumber = true
	deliver(c, c.SetContent("short\n"+strings.Repeat("word ", 20)+"\nlast\n", ".txt"))
	for _, line := range strings.Split(ansi.Strip(c.GetContent()), "\n") {
		if strings.HasSuffix(line, "  ") {
			t.Errorf("line padded: %q", line)
		}
	}
	var gutters []string
	for _, line := range strings.Split(ansi.Strip(c.View()), "\n") {
		if before, _, ok := strings.Cut(line, "│"); ok {
			gutters = append(gutters, strings.TrimSpace(before))
		}
	}
	want := []string{"1", "2", "", "", "", "3"}
	if len(gutters) < len(want) {
		t.Fatalf("view has %d gutter lines; want at least %d:\n%s", len(gutters), len(want), ansi.Strip(c.View()))
	}
	for i, w := range want {
		if gutters[i] != w {
			t.Fatalf("gutter %d = %q; want %q:\n%s", i, gutters[i], w, ansi.Strip(c.View()))
		}
	}
}

// A view that was never given content shows its NoContentStyle once it is
// sized, even when the width did not change.
func TestCodeShowsNoContentOnFirstResize(t *testing.T) {
	t.Parallel()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(80, 20)
	c.NoContentStyle = c.NoContentStyle.SetString("nothing-here")
	deliver(c, c.Update(tea.WindowSizeMsg{Width: 80, Height: 20}))
	if v := ansi.Strip(c.View()); !strings.Contains(v, "nothing-here") {
		t.Fatalf("view lacks the no-content text:\n%s", v)
	}
}

// A render that finishes after a newer one started is dropped.
func TestCodeDropsStaleRender(t *testing.T) {
	t.Parallel()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(80, 20)
	first := c.SetContent("first-content", ".txt")
	second := c.SetContent("second-content", ".txt")
	deliver(c, second)
	deliver(c, first)
	if v := ansi.Strip(c.View()); !strings.Contains(v, "second-content") || strings.Contains(v, "first-content") {
		t.Fatalf("a stale render replaced the current one:\n%s", v)
	}
	if c.Rendering() {
		t.Fatal("still rendering after the render arrived")
	}
}
