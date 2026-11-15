package code

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// renderLimit is how long one render of the inputs below may take. Shown
// as source, a render is bound by HighlightBudget and then shows plain text;
// rendered with glamour it is bound by glamour's own work, which takes at
// most about 0.15 s for these inputs. The limits leave a loaded CI runner
// room: they catch a render that does the work the limits are there to
// prevent, which takes seconds. The race detector makes glamour some twenty
// times slower.
func renderLimit(glamour bool) time.Duration {
	switch {
	case glamour && raceEnabled:
		return 20 * time.Second
	case glamour:
		return 2 * time.Second
	case raceEnabled:
		return 6 * time.Second
	default:
		return time.Second
	}
}

// renderTimed renders content as the file name, as markdown when glamour is
// set, and returns the rendered text and how long the render took.
func renderTimed(t *testing.T, content, name string, glamour bool) (string, time.Duration) {
	t.Helper()
	waitSlots(t)
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

// waitSlots waits for tokenisations left running by an earlier test, which
// hold tokenising slots.
func waitSlots(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for tokenising.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("tokenisations of earlier tests never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Code blocks cost chroma a language lookup each, about 2.4 ms for a
// language it does not know. A README renders its code blocks as plain
// text, and markdown shown as source is highlighted within a time budget,
// so neither takes long however many blocks it has or how they are
// written.
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
				if _, took := renderTimed(t, content, "README.md", glamour); took > renderLimit(glamour) {
					t.Fatalf("glamour %v: rendering took %v; want at most %v", glamour, took, renderLimit(glamour))
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
			if took > renderLimit(false) {
				t.Fatalf("rendering took %v; want at most %v", took, renderLimit(false))
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

// The code view's own highlighting matches what glamour's code block
// renderer produced before.
func TestHighlightMatchesGlamour(t *testing.T) {
	waitSlots(t)
	got, ok, err := highlight(common.StyleConfig(), "main.go", "package main\n\nfunc main() {}\n", false)
	if err != nil || !ok {
		t.Fatalf("highlight = %v, %v", ok, err)
	}
	want := "  \x1b[38;5;204mpackage\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;251mmain\x1b[0m\x1b[38;5;251m\x1b[0m\n" +
		"  \x1b[38;5;251m\x1b[0m\n" +
		"  \x1b[38;5;39mfunc\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;42mmain\x1b[0m\x1b[38;5;187m()\x1b[0m\x1b[38;5;251m \x1b[0m\x1b[38;5;187m{}\x1b[0m\x1b[38;5;251m\x1b[0m\n"
	if got != want {
		t.Fatalf("highlight =\n%q\nwant\n%q", got, want)
	}
}

// A script without a known name is highlighted by its content.
func TestHighlightByContent(t *testing.T) {
	waitSlots(t)
	script := "#!/bin/sh\nset -e\necho hi\n"
	out, ok, err := highlight(common.StyleConfig(), "setup", script, false)
	if err != nil || !ok {
		t.Fatalf("highlight = %v, %v", ok, err)
	}
	// Plain text gets the text colour throughout.
	plain, _, _ := highlight(common.StyleConfig(), "setup.txt", script, false)
	if out == plain {
		t.Fatal("a shell script without an extension was highlighted as plain text")
	}
}

// A lexer that tokenises all of its input up front cannot be stopped, so
// such files are highlighted only when small.
func TestEagerLexersCapped(t *testing.T) {
	waitSlots(t)
	small := "<script>\nlet a = 1;\n</script>\n<p>{a}</p>\n"
	if out, ok, _ := highlight(common.StyleConfig(), "app.svelte", small, false); !ok || !strings.Contains(out, "\x1b[") {
		t.Fatal("a small Svelte file was not highlighted")
	}
	large := strings.Repeat("<p>{a}</p>\n", (EagerHighlightMax/11)+1)
	start := time.Now()
	if _, ok, _ := highlight(common.StyleConfig(), "app.svelte", large, false); ok {
		t.Fatalf("a Svelte file of %d bytes was highlighted", len(large))
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("refusing a large Svelte file took %v", took)
	}
	if tokenising.Load() != 0 {
		t.Fatal("a refused file took a tokenising slot")
	}
}

// fakeLexer is a chroma lexer whose Tokenise runs tokenise.
type fakeLexer struct {
	tokenise func() (chroma.Iterator, error)
}

func (l fakeLexer) Config() *chroma.Config { return &chroma.Config{Name: "fake"} }

func (l fakeLexer) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	return l.tokenise()
}
func (l fakeLexer) SetRegistry(*chroma.LexerRegistry) chroma.Lexer { return l }
func (l fakeLexer) SetAnalyser(func(string) float32) chroma.Lexer  { return l }
func (l fakeLexer) AnalyseText(string) float32                     { return 0 }

// However many renders start at once, no more than maxTokenising
// tokenisations run, even when each runs past its budget; the others show
// plain text at once.
func TestTokenisingSlotsBounded(t *testing.T) {
	waitSlots(t)
	release := make(chan struct{})
	var running, most atomic.Int32
	slow := fakeLexer{tokenise: func() (chroma.Iterator, error) {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return chroma.Literator(), nil
	}}
	const calls = 16
	var wg sync.WaitGroup
	var refused atomic.Int32
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if _, ok, _ := tokenise(slow, "x"); !ok && time.Since(start) < HighlightBudget/2 {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := most.Load(); n > maxTokenising {
		t.Fatalf("%d tokenisations ran at once; want at most %d", n, maxTokenising)
	}
	if n := refused.Load(); n != calls-maxTokenising {
		t.Fatalf("%d calls were refused at once; want %d", n, calls-maxTokenising)
	}
	close(release)
	waitSlots(t)
}

// A lexer that panics fails the render, not the server, and frees its
// slot.
func TestTokenisePanicRecovered(t *testing.T) {
	waitSlots(t)
	boom := fakeLexer{tokenise: func() (chroma.Iterator, error) { panic("boom") }}
	_, ok, err := tokenise(boom, "x")
	if ok || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("tokenise = %v, %v; want the panic as an error", ok, err)
	}
	waitSlots(t)
	if n := tokenising.Load(); n != 0 {
		t.Fatalf("%d slots still taken", n)
	}
}
