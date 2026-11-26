package code

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/soft-serve/internal/testregexp"
	"github.com/charmbracelet/soft-serve/pkg/ui/syntax"
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
		styleConfig: syntax.StyleConfig(),
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

// lookups counts the times chroma's registry is searched for a lexer by
// file name or content, as it is for the language of a code block.
var lookups atomic.Int64

// lookupProbe is a lexer that counts the searches of the registry that
// reach it and matches nothing.
type lookupProbe struct{}

func (lookupProbe) Config() *chroma.Config {
	lookups.Add(1)
	return &chroma.Config{Name: "lookup probe"}
}

func (lookupProbe) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	return chroma.Literator(), nil
}

func (l lookupProbe) SetRegistry(*chroma.LexerRegistry) chroma.Lexer { return l }
func (l lookupProbe) SetAnalyser(func(string) float32) chroma.Lexer  { return l }

func (lookupProbe) AnalyseText(string) float32 {
	lookups.Add(1)
	return 0
}

// stalled is what the tokenisation by stallProbe waits for.
var stalled atomic.Pointer[chan struct{}]

// stallProbe is a lexer for files named *.stall that is blocked until the
// test releases it.
type stallProbe struct{ lookupProbe }

func (stallProbe) Config() *chroma.Config {
	return &chroma.Config{Name: "stall probe", Filenames: []string{"*.stall"}}
}

func (stallProbe) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	<-*stalled.Load()
	return chroma.Literator(), nil
}

func (stallProbe) AnalyseText(string) float32 { return 0 }

// The probes are registered before any test runs, so that no render reads
// the registry as it changes.
func init() {
	lexers.GlobalLexerRegistry.Register(lookupProbe{})
	lexers.GlobalLexerRegistry.Register(stallProbe{})
}

// render renders content as the file name, as markdown when glamour is set,
// within budget, and returns the rendered text. It first waits for the
// slots of earlier renders.
func render(t *testing.T, content, name string, glamour bool, budget syntax.Budget) string {
	t.Helper()
	uitest.WaitIdle(t, syntax.Tokenising)
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(120, 40)
	c.UseGlamour = glamour
	c.HighlightBudget = budget
	cmd := c.SetContent(content, name)
	for cmd != nil {
		msg := cmd()
		if _, ok := msg.(RenderedMsg); !ok {
			break
		}
		cmd = c.Update(msg)
	}
	return c.GetContent()
}

// highlighted reports whether rendered text has chroma's colours. Text shown
// plain, as when highlighting ran out of its budget, has none.
func highlighted(out string) bool { return strings.Contains(out, "\x1b[") }

// Code blocks cost chroma a language lookup each, about 2.4 ms for a
// language it does not know. A README renders its code blocks as plain
// text, without a lookup, and markdown shown as source is highlighted
// within a time budget, so neither takes long however many blocks it has or
// how they are written. Highlighting the blocks of these READMEs made five
// lookups a block.
//
// Neither is timed. Rendered as markdown, the lookups are counted. Shown as
// source, these inputs take at most 0.8 s even without the budget, so a
// time limit here could not tell the budget is there: the budget is
// TestBudgetShowsPlain's and TestDefaultBudgetShowsPlain's check, and the
// render as source is only checked to show the content.
func TestCodeBlocksRenderQuickly(t *testing.T) {
	cases := map[string]string{
		"indented code before markers": strings.Repeat("    code\n>\n", 500),
		"indented fence":               "    ```\n" + strings.Repeat("#\n    code\n", 499),
		"invalid opener":               strings.Repeat("``` `\nx\n", 500),
		"fences in block quotes":       strings.Repeat("> ```\n\n", 400),
		"fences in list items":         strings.Repeat("- ```\n", 700),
		"empty fences":                 strings.Repeat("```\n```\n", 500),
		"indented code and headings":   strings.Repeat("    code\n# h\n", 500),
		"unknown languages":            strings.Repeat("```zzz\nx\n```\n", 333),
	}
	for name, content := range cases {
		for _, glamour := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				if !glamour {
					if out := render(t, content, "README.md", false, nil); strings.TrimSpace(out) == "" {
						t.Fatal("rendering as source lost the content")
					}
					return
				}
				// A render as source before may still be finishing
				// its last token, and the README's own name is looked
				// up.
				uitest.WaitIdle(t, syntax.Tokenising)
				lookups.Store(0)
				render(t, content, "README.md", true, nil)
				if n := lookups.Load(); n > 10 {
					t.Fatalf("rendering as markdown searched the registry %d times; want no search per code block", n)
				}
			})
		}
	}
}

// renderStalled starts the render of a file whose lexer is blocked until
// the test ends, and returns where its text will come.
func renderStalled(t *testing.T, budget syntax.Budget) <-chan string {
	t.Helper()
	uitest.WaitIdle(t, syntax.Tokenising)
	release := make(chan struct{})
	stalled.Store(&release)
	t.Cleanup(func() {
		close(release)
		uitest.WaitIdle(t, syntax.Tokenising)
	})
	c := New(uitest.NewCommon(t, context.Background(), nil, nil), "", "")
	c.SetSize(120, 40)
	if budget != nil { // nil leaves New's
		c.HighlightBudget = budget
	}
	cmd := c.SetContent("stalled line\n", "x.stall")
	done := make(chan string, 1)
	go func() {
		c.Update(cmd())
		done <- c.GetContent()
	}()
	return done
}

// wantPlain fails the test unless the stalled render comes back plain, with
// its content, while the lexer is still blocked.
func wantPlain(t *testing.T, done <-chan string) {
	t.Helper()
	out, ok := uitest.Receive(done)
	if !ok {
		t.Fatalf("the render was not back after %v; want it back once its budget ended", uitest.HangGuard)
	}
	if syntax.Tokenising() == 0 || highlighted(out) || !strings.Contains(out, "stalled line") {
		t.Fatalf("rendered %q; want the content plain, with the lexer still running", out)
	}
}

// A render waits on the budget it is given and is plain once that ends.
func TestBudgetShowsPlain(t *testing.T) {
	budget := make(chan time.Time)
	done := renderStalled(t, func() <-chan time.Time { return budget })
	if !uitest.WaitFor(func() bool {
		select {
		case budget <- time.Time{}: // taken only if the render waits on it
			return true
		default:
			return false
		}
	}) {
		t.Fatal("the render did not wait on the budget it was given")
	}
	wantPlain(t, done)
}

// Without a budget, as in production, a render is plain once
// syntax.HighlightBudget has passed, and no sooner.
func TestDefaultBudgetShowsPlain(t *testing.T) {
	start := time.Now()
	wantPlain(t, renderStalled(t, nil))
	if took := time.Since(start); took < syntax.HighlightBudget {
		t.Fatalf("the render took %v; want it to wait %v for the lexer", took, syntax.HighlightBudget)
	}
}

// A file in a lexer that tokenises up front is plain past
// syntax.EagerHighlightMax, without a budget to cut it off.
func TestEagerLexerShowsPlain(t *testing.T) {
	const line = "<script lang=\"zzz\">x</script>\n"
	content := strings.Repeat(line, syntax.EagerHighlightMax/len(line)+1)
	if out := render(t, content, "x.svelte", false, uitest.Unbounded); highlighted(out) || !strings.Contains(out, "zzz") {
		t.Fatal("rendered highlighted, or without the content; want plain text past the cap")
	}
}

// Ordinary files are highlighted: a Go file, and a README with a Go code
// block shown as source. The renders have no budget: with one, a loaded
// machine shows them plain.
func TestOrdinaryFilesHighlighted(t *testing.T) {
	goFile := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if out := render(t, strings.Repeat(goFile, 20), "main.go", false, uitest.Unbounded); !highlighted(out) {
		t.Fatal("a Go file was not highlighted")
	}
	readme := "# Title\n\nSome text.\n\n```go\n" + goFile + "```\n"
	if out := render(t, readme, "README.md", false, uitest.Unbounded); !strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README shown as source was not highlighted:\n%q", out)
	}
	// Rendered as markdown, the README keeps its code, styled but not
	// highlighted.
	if out := render(t, readme, "README.md", true, uitest.Unbounded); !strings.Contains(out, "Println") || strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README was not shown as plain code:\n%q", out)
	}
}

// Whether the content is markdown is worked out once per SetContent, not on
// every render, and is worked out again for new content.
func TestMarkdownCachedPerContent(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	r := New(c, "", "")
	r.SetSize(80, 24)
	r.UseGlamour = true

	deliver(r, r.SetContent("# Title\n\nbody\n", "README.md"))
	if r.isMarkdown == nil || !*r.isMarkdown {
		t.Fatalf("README.md not cached as markdown")
	}
	cached := r.isMarkdown
	r.SetSize(60, 20)
	deliver(r, r.Init())
	if r.isMarkdown != cached {
		t.Error("markdown detection ran again on a re-render")
	}

	deliver(r, r.SetContent("package main\n", "main.go"))
	if r.isMarkdown == nil || *r.isMarkdown {
		t.Error("main.go still treated as markdown after SetContent")
	}
}

// SetMarkdownContent takes the caller's answer and does not work it out
// again, even on a re-render.
func TestSetMarkdownContentUsesCallerFlag(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	r := New(c, "", "")
	r.SetSize(80, 24)
	r.UseGlamour = true

	// README.md would be detected as markdown; the caller's false must win.
	r.SetMarkdownContent("# Title\n\nbody\n", "README.md", false)
	if r.markdown() {
		t.Fatal("SetMarkdownContent flag false was replaced by detection")
	}
	r.SetSize(60, 20)
	r.Init()
	if r.markdown() {
		t.Error("markdown detection ran again on a re-render")
	}

	// SetContent still detects for callers that pass no flag.
	r.SetContent("# Title\n\nbody\n", "README.md")
	if !r.markdown() {
		t.Error("SetContent did not detect README.md as markdown")
	}
}

// These tests check chroma's colours, which must not depend on its regex
// timeout.
func init() { testregexp.DisableTimeouts() }
