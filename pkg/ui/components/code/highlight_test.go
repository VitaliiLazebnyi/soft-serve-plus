package code

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

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

// stall is one tokenisation by stallProbe: at its first token it offers
// budget its end, waits to be released whether that is taken or not, and
// closes left.
type stall struct {
	budget   chan time.Time
	released chan struct{}
	release  func()
	left     chan struct{}
	// ended is whether budget's end was taken; read it once left is closed.
	ended bool
}

// newStall returns the stall the next tokenisation by stallProbe runs. The
// end of the test releases it, however the test ends, and waits for it.
func newStall(t *testing.T) *stall {
	t.Helper()
	waitTokenisations(t)
	s := &stall{budget: make(chan time.Time), released: make(chan struct{}), left: make(chan struct{})}
	s.release = sync.OnceFunc(func() { close(s.released) })
	stalling.Store(s)
	t.Cleanup(func() {
		s.release()
		waitTokenisations(t)
	})
	return s
}

// render renders a file of the stall's with budget as the highlighting
// budget, and checks that it comes back plain while its tokenisation is
// still stalled.
func (s *stall) render(t *testing.T, budget func() <-chan time.Time) {
	t.Helper()
	content := strings.Repeat("stalled line\n", 3)
	done := make(chan string, 1)
	go func() { done <- renderBudget(t, content, "x.stall", false, budget) }()
	out, ok := await(done)
	if !ok {
		t.Fatalf("the render was not back after %v; want it back once its budget ended", hangGuard)
	}
	// This also fails if waitTokenisations can no longer see a tokenisation.
	if !tokenisationRunning() {
		t.Fatal("the render was back with no tokenisation running; want it stalled")
	}
	s.release()
	if _, ok := await(s.left); !ok {
		t.Fatalf("the stalled tokenisation did not run and end within %v of its release", hangGuard)
	}
	if highlighted(out) {
		t.Fatal("rendered highlighted; want plain text once the budget ended")
	}
	if !strings.Contains(out, "stalled line") {
		t.Fatalf("the content is missing:\n%q", out)
	}
}

// stalling is the stall the next tokenisation by stallProbe runs.
var stalling atomic.Pointer[stall]

// stallProbe is a lexer for files named *.stall whose tokenisation runs
// past its budget: it runs the stall in stalling.
type stallProbe struct{}

func (stallProbe) Config() *chroma.Config {
	return &chroma.Config{Name: "stall probe", Filenames: []string{"*.stall"}}
}

func (stallProbe) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	s := stalling.Load()
	return func() chroma.Token {
		select {
		case s.budget <- time.Time{}:
			s.ended = true
			<-s.released
		case <-s.released:
		}
		close(s.left)
		return chroma.EOF
	}, nil
}

func (l stallProbe) SetRegistry(*chroma.LexerRegistry) chroma.Lexer { return l }
func (l stallProbe) SetAnalyser(func(string) float32) chroma.Lexer  { return l }
func (stallProbe) AnalyseText(string) float32                       { return 0 }

// The probes are registered before any test runs, so that no render reads
// the registry as it changes.
func init() {
	lexers.GlobalLexerRegistry.Register(lookupProbe{})
	lexers.GlobalLexerRegistry.Register(stallProbe{})
}

// unbounded is a highlighting budget that never ends.
func unbounded() <-chan time.Time { return nil }

// expired is a highlighting budget that has ended.
func expired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// render renders content as the file name, as markdown when glamour is set,
// with no highlighting budget, and returns the rendered text. No test
// depends on how fast the machine highlights.
func render(t *testing.T, content, name string, glamour bool) string {
	t.Helper()
	waitTokenisations(t)
	return renderBudget(t, content, name, glamour, unbounded)
}

// renderBudget is render with budget as the highlighting budget.
func renderBudget(t *testing.T, content, name string, glamour bool, budget func() <-chan time.Time) string {
	t.Helper()
	c := New(common.NewCommon(context.Background(), 120, 40), "", "")
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

// hangGuard is how long a test waits for work that ends at once when the
// code is right, before it fails rather than wait for the test binary's
// timeout.
const hangGuard = time.Minute

// await returns what c gives, or false once it has given nothing for
// hangGuard. It waits a second at a time, so that a pause of the process
// costs one second of the guard, not all of it.
func await[T any](c <-chan T) (T, bool) {
	for range hangGuard / time.Second {
		select {
		case v := <-c:
			return v, true
		case <-time.After(time.Second):
		}
	}
	var none T
	return none, false
}

// tokenisationRunning reports whether a goroutine of package common's
// tokeniseUntil is left. Its slots cannot be seen from this package.
func tokenisationRunning() bool {
	buf := make([]byte, 64<<10)
	for {
		if n := runtime.Stack(buf, true); n < len(buf) {
			return bytes.Contains(buf[:n], []byte("pkg/ui/common.tokeniseUntil"))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitTokenisations waits for the tokenisations earlier renders left
// running. Each holds a slot until it ends, also once its budget is over,
// and a render that finds no slot free is plain without running its lexer.
func waitTokenisations(t *testing.T) {
	t.Helper()
	const poll = 10 * time.Millisecond
	// The polls are counted, as in await.
	for range hangGuard / poll {
		if !tokenisationRunning() {
			return
		}
		time.Sleep(poll)
	}
	t.Fatal("tokenisations of earlier renders never ended")
}

// Code blocks cost chroma a language lookup each, about 2.4 ms for a
// language it does not know. A README renders its code blocks as plain
// text, without a lookup, and markdown shown as source is highlighted
// within a time budget, so neither takes long however many blocks it has or
// how they are written. Highlighting the blocks of these READMEs made five
// lookups a block.
//
// Neither is timed. Rendered as markdown, the lookups are counted. Shown as
// source, the render is only checked to show the content, highlighted or
// not: its budget has ended before it starts, so that it takes no longer
// than the budget would let it. TestBudgetShowsPlain checks that the budget
// ends a render, TestDefaultBudgetShowsPlain that there is one by default.
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
					if out := renderBudget(t, content, "README.md", false, expired); strings.TrimSpace(out) == "" {
						t.Fatal("rendering as source lost the content")
					}
					return
				}
				// A render as source before may still be finishing
				// its last token. The README's own name is looked up.
				waitTokenisations(t)
				lookups.Store(0)
				render(t, content, "README.md", true)
				if n := lookups.Load(); n > 10 {
					t.Fatalf("rendering as markdown searched the registry %d times; want no search per code block", n)
				}
			})
		}
	}
}

// A file whose highlighting runs past its budget is shown as plain text,
// with its content. The stall probe's tokenisation ends the budget and waits
// until the render is back, so the budget is what ends the render. Lexers
// that look embedded languages up, which take seconds over a file, are
// stopped by the budget the same way; TestBudgetStopsEmbeddedLanguages in
// package common checks that.
func TestBudgetShowsPlain(t *testing.T) {
	s := newStall(t)
	s.render(t, func() <-chan time.Time { return s.budget })
	if !s.ended {
		t.Fatal("the render did not wait on the budget it was given")
	}
}

// Without a HighlightBudget, as in production, a render is given up once
// common.HighlightBudget has passed. The stall has no end of its own, so
// only that budget can end the render, and load can only lengthen the time
// measured, which has only a lower bound.
func TestDefaultBudgetShowsPlain(t *testing.T) {
	s := newStall(t)
	start := time.Now()
	s.render(t, nil)
	if took := time.Since(start); took < common.HighlightBudget {
		t.Fatalf("the render took %v; want it to wait %v for the lexer", took, common.HighlightBudget)
	}
}

// A file in a lexer that tokenises up front, which the budget cannot stop,
// is shown as plain text past common.EagerHighlightMax, without a budget.
func TestEagerLexerShowsPlain(t *testing.T) {
	const line = "<script lang=\"zzz\">x</script>\n"
	content := strings.Repeat(line, common.EagerHighlightMax/len(line)+1)
	out := render(t, content, "x.svelte", false)
	if highlighted(out) {
		t.Fatal("rendered highlighted; want plain text past the cap")
	}
	if !strings.Contains(out, "zzz") {
		t.Fatal("the content is missing")
	}
}

// Ordinary files are highlighted: a Go file, and a README with a Go code
// block shown as source. The render has no budget: with one, a loaded
// machine shows them plain.
func TestOrdinaryFilesHighlighted(t *testing.T) {
	goFile := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if out := render(t, strings.Repeat(goFile, 20), "main.go", false); !highlighted(out) {
		t.Fatal("a Go file was not highlighted")
	}
	readme := "# Title\n\nSome text.\n\n```go\n" + goFile + "```\n"
	if out := render(t, readme, "README.md", false); !strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README shown as source was not highlighted:\n%q", out)
	}
	// Rendered as markdown, the README keeps its code, styled but not
	// highlighted.
	if out := render(t, readme, "README.md", true); !strings.Contains(out, "Println") || strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README was not shown as plain code:\n%q", out)
	}
}
