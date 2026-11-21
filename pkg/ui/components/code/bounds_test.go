package code

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// renderWithin renders content as a README the way the Readme tab does and
// fails unless the render, and Update, finish within uitest.HangGuard. It
// returns the view's content without styles.
func renderWithin(t *testing.T, content string) string {
	t.Helper()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(120, 40)
	c.UseGlamour = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := c.SetContent(content, "README.md")
		for cmd != nil {
			msg := cmd()
			if _, ok := msg.(RenderedMsg); !ok {
				return
			}
			cmd = c.Update(msg)
		}
	}()
	if _, ok := uitest.Receive(done); !ok {
		t.Fatalf("rendering did not finish within %v", uitest.HangGuard)
	}
	return ansi.Strip(c.GetContent())
}

// glamourPadded reports whether a line of out that has text is padded with
// spaces to the width, as each line glamour writes is and no line of plain
// text is.
func glamourPadded(out string) bool {
	for l := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(l) != "" && strings.HasSuffix(l, "  ") {
			return true
		}
	}
	return false
}

// glamour's time grows fast with the length of a code token, the number of
// blocks and the nesting depth. A README past its limits is shown as plain
// text, quickly.
func TestGlamourBounded(t *testing.T) {
	t.Parallel()
	var nested strings.Builder
	for range 100 {
		for i := range 20 {
			nested.WriteString(strings.Repeat("  ", i) + "- x\n")
		}
	}
	cases := map[string]string{
		"long token in a code block": "# T\n\n```js\n" + strings.Repeat("a", 200<<10) + "\n```\n",
		"long token, small file":     "# T\n\n```js\n" + strings.Repeat("a", 30<<10) + "\n```\n",
		"many short paragraphs":      strings.Repeat("a\n\n", (240<<10)/3),
		"deep block quote":           strings.Repeat(">", 128) + " x\n",
		"deep lists":                 nested.String(),
		"many lines":                 strings.Repeat("- item\n", 5000),
		"ordered markers":            strings.Repeat("1. ", 200) + "x\n",
		"ordered markers, parens":    strings.Repeat("1) ", 200) + "x\n",
		"many nested markers":        strings.Repeat("- - - - x\n", 1000),
	}
	// Each case takes at most 0.45 s on a loaded machine under the race
	// detector; rendered as markdown, many short paragraphs took 9.5 s and
	// the deep block quote minutes. The render is not timed: the view
	// itself tells whether the limits applied. Shown as plain text, it has
	// the content's first line and no line padded out to the width, as
	// glamour pads every line it writes. renderWithin's minute only stops a
	// render that hangs.
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := renderWithin(t, content)
			first := strings.SplitN(strings.TrimSpace(content), "\n", 2)[0]
			if len(first) > 60 {
				first = first[:60]
			}
			if !strings.Contains(out, first) || glamourPadded(out) {
				t.Fatalf("not shown as plain text: lacks %q, or has glamour's padding", first)
			}
		})
	}

	// A README within the limits is still rendered as markdown.
	if out := renderWithin(t, "# Title\n\nSome *text*.\n"); strings.Contains(out, "# Title") || !strings.Contains(out, "Title") || !glamourPadded(out) {
		t.Fatalf("a small README was not rendered as markdown:\n%s", out)
	}
}

// glamour's output is wrapped line by line: wrapping it whole runs every
// byte of it through the style-keeping wrap writer, which allocates once a
// byte. The render of this README makes about 1.8 million allocations
// wrapped per line and 3 million wrapped whole.
//
// Not parallel: it measures the bytes the process allocates.
func TestGlamourOutputWrappedPerLine(t *testing.T) {
	md := true
	j := renderJob{
		content:     strings.Repeat("Some words in a paragraph that goes on for a while.\n\n", 450),
		extension:   "README.md",
		isMarkdown:  &md,
		glamour:     true,
		width:       116,
		tabWidth:    defaultTabWidth,
		styleConfig: common.StyleConfig(),
	}
	if msg := j.run(); len(msg.lines) < 900 || msg.lineNumbers {
		t.Fatalf("the README was not rendered as markdown: %d lines", len(msg.lines))
	}
	if n := testing.AllocsPerRun(2, func() { j.run() }); n > 2_400_000 {
		t.Fatalf("rendering the README made %.0f allocations; want at most 2.4 million", n)
	}
}

// Option changes and resizes while a render is in flight start no render of
// their own; one render, for what is wanted when the running one returns,
// follows it.
func TestCodeOneRenderInFlight(t *testing.T) {
	t.Parallel()
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(80, 20)
	c.ShowLineNumber = true
	first := c.SetContent("line\n", ".txt")
	if first == nil {
		t.Fatal("no render started")
	}
	for i := range 40 {
		c.ShowLineNumber = !c.ShowLineNumber
		if cmd := c.Init(); cmd != nil {
			t.Fatalf("toggle %d started a render while one was in flight", i)
		}
		if cmd := c.Update(tea.WindowSizeMsg{Width: 60 + i%2, Height: 20}); cmd != nil {
			if _, ok := cmd().(RenderedMsg); ok {
				t.Fatalf("resize %d started a render while one was in flight", i)
			}
		}
	}
	next := c.Update(first())
	if next == nil {
		t.Fatal("no render followed the stale one")
	}
	msg, ok := next().(RenderedMsg)
	if !ok {
		t.Fatal("the render that followed sent no RenderedMsg")
	}
	if cmd := c.Update(msg); cmd != nil {
		t.Fatal("the last render started another")
	}
	if c.Rendering() || !strings.Contains(ansi.Strip(c.View()), "line") {
		t.Fatalf("the content is not shown:\n%s", ansi.Strip(c.View()))
	}
}
