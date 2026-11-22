package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

// renderAllocMax is the most bytes the render of a file of 1 MiB may
// allocate, whatever the width. Rendering it inside Update used to allocate
// gigabytes, more the wider the terminal.
const renderAllocMax = 160 << 20

// newRenderFiles returns a Files pane width columns wide that has seen a
// window size message.
func newRenderFiles(width int) *Files {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, width, 40))
	f.SetSize(width, 40)
	f.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	return f
}

// openFile hands f a file's content and returns the command Update returned,
// failing if Update allocated more than updateAllocMax.
func openFile(t *testing.T, f *Files, content, name string) tea.Cmd {
	t.Helper()
	msg := FileContentMsg{content: content, ext: name, path: name, load: f.load, isMarkdown: common.IsFileMarkdown(content, name)}
	var cmd tea.Cmd
	if n := allocated(func() { cmd = f.Update(msg) }); n > updateAllocMax {
		t.Fatalf("Update allocated %d bytes to open %s; want at most %d", n, name, updateAllocMax)
	}
	return cmd
}

// A file of 1 MiB of newlines is rendered in a command with bounded memory
// at any width, its lines are numbered as they are shown rather than all at
// once, and toggling the numbers does not render it in Update.
func TestFileOfNewlinesRendersOutsideUpdate(t *testing.T) {
	content := strings.Repeat("\n", 1<<20)
	for _, width := range []int{100, common.MaxWidth} {
		f := newRenderFiles(width)
		cmd := openFile(t, f, content, "empty.txt")
		var n int
		if a := allocated(func() { n = deliverAll(f, cmd) }); a > renderAllocMax {
			t.Fatalf("width %d: the render allocated %d bytes; want at most %d", width, a, renderAllocMax)
		}
		if n == 0 {
			t.Fatalf("width %d: no render result arrived as a message", width)
		}
		if f.activeView != filesViewContent {
			t.Fatalf("width %d: view = %v; want the file", width, f.activeView)
		}
		if strings.Contains(f.code.GetContent(), "│") {
			t.Fatalf("width %d: every line was numbered up front", width)
		}
		if v := ansi.Strip(f.code.View()); !strings.Contains(v, " 1 │") {
			t.Fatalf("width %d: the shown lines have no numbers:\n%s", width, v)
		}

		// "l" hides the line numbers, then shows them again.
		for _, numbered := range []bool{false, true} {
			var cmd tea.Cmd
			if a := allocated(func() { cmd = f.Update(tea.KeyPressMsg{Code: 'l', Text: "l"}) }); a > updateAllocMax {
				t.Fatalf("width %d: Update allocated %d bytes for l; want at most %d", width, a, updateAllocMax)
			}
			if a := allocated(func() { deliverAll(f, cmd) }); a > renderAllocMax {
				t.Fatalf("width %d: the render for l allocated %d bytes; want at most %d", width, a, renderAllocMax)
			}
			if got := strings.Contains(ansi.Strip(f.code.View()), " 1 │"); got != numbered {
				t.Fatalf("width %d: line numbers shown = %v; want %v", width, got, numbered)
			}
		}
	}
}

// A JavaScript file that is one identifier of 1 MiB took minutes to
// highlight. It is shown without highlighting, in a command.
func TestFileWithLongLineNotHighlighted(t *testing.T) {
	f := newRenderFiles(120)
	done := make(chan tea.Cmd, 1)
	go func() {
		done <- f.Update(FileContentMsg{content: strings.Repeat("a", 1<<20), ext: "big.js", path: "big.js", load: f.load})
	}()
	var cmd tea.Cmd
	select {
	case cmd = <-done:
	case <-time.After(time.Minute):
		t.Fatal("Update did not return within a minute for a 1 MiB identifier")
	}
	rendered := make(chan struct{})
	go func() { deliverAll(f, cmd); close(rendered) }()
	select {
	case <-rendered:
	case <-time.After(time.Minute):
		t.Fatal("the render did not finish within a minute for a 1 MiB identifier")
	}
	if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "aaaa") {
		t.Fatal("the long line was highlighted, or is missing")
	}
}

// Ordinary files are highlighted; files over 256 KiB are not. The renders
// have no highlighting budget, so that a loaded machine does not show the
// small file plain.
func TestFileHighlightLimit(t *testing.T) {
	unbounded := func() <-chan time.Time { return nil }
	small := "package main\n\nfunc main() {}\n"
	f := newRenderFiles(120)
	f.code.HighlightBudget = unbounded
	deliverAll(f, openFile(t, f, small, "main.go"))
	if !strings.Contains(f.code.GetContent(), "\x1b[") {
		t.Fatal("a small Go file was not highlighted")
	}

	// Long enough lines that only the size is over a limit: there are fewer
	// than code.HighlightMaxLines of them.
	const line = "func f() {} // one of fewer than ten thousand lines\n"
	large := strings.Repeat(line, (256<<10)/len(line)+1)
	f = newRenderFiles(120)
	f.code.HighlightBudget = unbounded
	deliverAll(f, openFile(t, f, large, "large.go"))
	if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "func f() {}") {
		t.Fatal("a Go file over 256 KiB was highlighted, or is missing")
	}
}
