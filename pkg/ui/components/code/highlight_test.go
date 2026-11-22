package code

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
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
func render(t *testing.T, content, name string, glamour bool, budget common.Budget) string {
	t.Helper()
	uitest.WaitIdle(t, common.Tokenising)
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
				uitest.WaitIdle(t, common.Tokenising)
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
func renderStalled(t *testing.T, budget common.Budget) <-chan string {
	t.Helper()
	uitest.WaitIdle(t, common.Tokenising)
	release := make(chan struct{})
	stalled.Store(&release)
	t.Cleanup(func() {
		close(release)
		uitest.WaitIdle(t, common.Tokenising)
	})
	c := New(common.NewCommon(context.Background(), 120, 40), "", "")
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
	if common.Tokenising() == 0 || highlighted(out) || !strings.Contains(out, "stalled line") {
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
// common.HighlightBudget has passed, and no sooner.
func TestDefaultBudgetShowsPlain(t *testing.T) {
	start := time.Now()
	wantPlain(t, renderStalled(t, nil))
	if took := time.Since(start); took < common.HighlightBudget {
		t.Fatalf("the render took %v; want it to wait %v for the lexer", took, common.HighlightBudget)
	}
}

// A file in a lexer that tokenises up front is plain past
// common.EagerHighlightMax, without a budget to cut it off.
func TestEagerLexerShowsPlain(t *testing.T) {
	const line = "<script lang=\"zzz\">x</script>\n"
	content := strings.Repeat(line, common.EagerHighlightMax/len(line)+1)
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
