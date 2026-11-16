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
)

// sourceLimit is how long rendering one of the inputs below as source may
// take. The render is bound by HighlightBudget and then shows plain text: it
// takes about 0.2 s, and 0.35 s on a loaded machine, race detector or not.
// Without the budget, the embedded languages take about 6 s, and 12 s under
// the race detector.
func sourceLimit() time.Duration {
	if raceEnabled {
		return 6 * time.Second
	}
	return 3 * time.Second
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

// The probe is registered before any test runs, so that no render reads
// the registry as it changes.
func init() {
	lexers.GlobalLexerRegistry.Register(lookupProbe{})
}

// renderTimed renders content as the file name, as markdown when glamour is
// set, and returns the rendered text and how long the render took.
func renderTimed(t *testing.T, content, name string, glamour bool) (string, time.Duration) {
	t.Helper()
	c := New(common.NewCommon(context.Background(), 120, 40), "", "")
	c.UseGlamour = glamour
	start := time.Now()
	cmd := c.SetContent(content, name)
	for cmd != nil {
		msg := cmd()
		if _, ok := msg.(RenderedMsg); !ok {
			break
		}
		cmd = c.Update(msg)
	}
	return c.GetContent(), time.Since(start)
}

// Code blocks cost chroma a language lookup each, about 2.4 ms for a
// language it does not know. A README renders its code blocks as plain
// text, without a lookup, and markdown shown as source is highlighted
// within a time budget, so neither takes long however many blocks it has or
// how they are written. Highlighting the blocks of these READMEs made five
// lookups a block. TestEmbeddedLanguagesRenderQuickly has inputs that take
// seconds without the budget; these take at most 0.8 s.
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
					if _, took := renderTimed(t, content, "README.md", false); took > sourceLimit() {
						t.Fatalf("rendering as source took %v; want at most %v", took, sourceLimit())
					}
					return
				}
				// A render as source before may still be finishing
				// its last token, and the README's own name is looked
				// up.
				lookups.Store(0)
				renderTimed(t, content, "README.md", true)
				if n := lookups.Load(); n > 10 {
					t.Fatalf("rendering as markdown searched the registry %d times; want no search per code block", n)
				}
			})
		}
	}
}

// Other lexers look embedded languages up the same way. Files in them render
// within the budget, as plain text when highlighting would take longer.
func TestEmbeddedLanguagesRenderQuickly(t *testing.T) {
	cases := map[string]string{
		"x.gmi":    strings.Repeat("```zzz\nx\n```\n", 2500),
		"x.svelte": strings.Repeat("<script lang=\"zzz\">x</script>\n", 2500),
		"x.rst":    strings.Repeat(".. code-block:: zzz\n\n   x\n\n", 2500),
		"x.org":    strings.Repeat("#+begin_src zzz\nx\n#+end_src\n", 2500),
		"x.raku":   strings.Repeat("=begin code :lang<zzz>\nx\n=end code\n", 2500),
		"x.art":    strings.Repeat("{!zzz x }\n", 2500),
		"x.md":     strings.Repeat("```zzz\nx\n```\n", 2500),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			out, took := renderTimed(t, content, name, false)
			if took > sourceLimit() {
				t.Fatalf("rendering took %v; want at most %v", took, sourceLimit())
			}
			if !strings.Contains(out, "zzz") {
				t.Fatal("the content is missing")
			}
		})
	}
}

// The budget leaves ordinary files highlighted: a Go file, and a README with
// a Go code block shown as source.
func TestOrdinaryFilesHighlighted(t *testing.T) {
	goFile := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if out, _ := renderTimed(t, strings.Repeat(goFile, 20), "main.go", false); !strings.Contains(out, "\x1b[") {
		t.Fatal("a Go file was not highlighted")
	}
	readme := "# Title\n\nSome text.\n\n```go\n" + goFile + "```\n"
	if out, _ := renderTimed(t, readme, "README.md", false); !strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README shown as source was not highlighted:\n%q", out)
	}
	// Rendered as markdown, the README keeps its code, styled but not
	// highlighted.
	if out, _ := renderTimed(t, readme, "README.md", true); !strings.Contains(out, "Println") || strings.Contains(out, "\x1b[38;5;39mfunc") {
		t.Fatalf("the Go block of a README was not shown as plain code:\n%q", out)
	}
}
