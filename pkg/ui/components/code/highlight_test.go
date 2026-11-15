package code

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// renderLimit is how long one render of the inputs below may take: the
// highlight budget, glamour's own work, and room for a loaded machine. The
// race detector makes glamour's work some twenty times slower.
func renderLimit() time.Duration {
	if raceEnabled {
		return 6 * time.Second
	}
	return 500 * time.Millisecond
}

// renderTimed renders content as the file name, as markdown when glamour is
// set, and returns the rendered text and how long the render took.
func renderTimed(t *testing.T, content, name string, glamour bool) (string, time.Duration) {
	t.Helper()
	waitOverrun(t)
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

// waitOverrun waits for tokenisations left running by an earlier test, which
// keep tokenise from starting new ones.
func waitOverrun(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for overrun.Load() > 0 {
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
				if _, took := renderTimed(t, content, "README.md", glamour); took > renderLimit() {
					t.Fatalf("glamour %v: rendering took %v; want at most %v", glamour, took, renderLimit())
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
			if took > renderLimit() {
				t.Fatalf("rendering took %v; want at most %v", took, renderLimit())
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
	waitOverrun(t)
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
