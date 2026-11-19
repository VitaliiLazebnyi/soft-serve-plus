package code

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

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

// The probe is registered before any test runs, so that no render reads
// the registry as it changes.
func init() {
	lexers.GlobalLexerRegistry.Register(lookupProbe{})
}

// render renders content as the file name, as markdown when glamour is set,
// and returns the rendered text.
func render(t *testing.T, content, name string, glamour bool) string {
	t.Helper()
	c := New(common.NewCommon(context.Background(), 120, 40), "", "")
	c.UseGlamour = glamour
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
// TestEmbeddedLanguagesRenderQuickly's check, with inputs that take seconds
// without it, and the render as source is only checked to show the content.
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
					if out := render(t, content, "README.md", false); strings.TrimSpace(out) == "" {
						t.Fatal("rendering as source lost the content")
					}
					return
				}
				// A render as source before may still be finishing
				// its last token, and the README's own name is looked
				// up.
				lookups.Store(0)
				render(t, content, "README.md", true)
				if n := lookups.Load(); n > 10 {
					t.Fatalf("rendering as markdown searched the registry %d times; want no search per code block", n)
				}
			})
		}
	}
}

// Other lexers look embedded languages up the same way. Files in them render
// within the budget, as plain text when highlighting would take longer.
//
// The render is not timed: a render that shows these inputs plain was cut
// off by the budget, or by the cap on lexers the budget cannot stop
// (x.svelte); without the budget they come back highlighted. Each input
// repeats its block up to HighlightMaxLines, the most lines highlighted at
// all, so that highlighting it whole takes as long as an input can: 7 s
// (x.rst) to 15 s (x.art), 35 to 75 times the budget, where 2500 blocks
// took x.art only 1 s. A machine would have to be that much faster for one
// to fit the budget; one ten times faster, a 2 s budget, still leaves them
// all plain. A loaded machine tokenises less within the budget, which only
// makes the text plainer.
func TestEmbeddedLanguagesRenderQuickly(t *testing.T) {
	// fill repeats block, whole lines, up to HighlightMaxLines lines.
	fill := func(block string) string {
		return strings.Repeat(block, HighlightMaxLines/strings.Count(block, "\n"))
	}
	cases := map[string]string{
		"x.gmi":    fill("```zzz\nx\n```\n"),
		"x.svelte": fill("<script lang=\"zzz\">x</script>\n"),
		"x.rst":    fill(".. code-block:: zzz\n\n   x\n\n"),
		"x.org":    fill("#+begin_src zzz\nx\n#+end_src\n"),
		"x.raku":   fill("=begin code :lang<zzz>\nx\n=end code\n"),
		"x.art":    fill("{!zzz x }\n"),
		"x.md":     fill("```zzz\nx\n```\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			out := render(t, content, name, false)
			if highlighted(out) {
				t.Fatal("rendered highlighted; want plain text once the budget ran out")
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
