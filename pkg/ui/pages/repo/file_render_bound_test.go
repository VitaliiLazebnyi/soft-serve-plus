package repo

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
)

// codeRenders runs cmd and returns the code view renders it produces.
func codeRenders(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	for _, m := range runCmd(cmd) {
		if _, ok := m.(code.RenderedMsg); ok {
			out = append(out, m)
		}
	}
	return out
}

// A file of many short lines is not highlighted, however small: chroma's
// cost grows with its tokens.
func TestFileOfManyLinesNotHighlighted(t *testing.T) {
	f := newUnboundedFiles(t)
	deliverAll(f, openFile(t, f, strings.Repeat("x := 1\n", 20000), "many.go"))
	if c := f.code.GetContent(); strings.Contains(c, "\x1b[") || !strings.Contains(c, "x := 1") {
		t.Fatal("a Go file of 20,000 lines was highlighted, or is missing")
	}
}

// Pressing "l" over and over on a large file renders it once at a time:
// presses while a render runs start none, and one render follows it.
func TestFileLineNumberTogglesRenderOneAtATime(t *testing.T) {
	f := newRenderFiles(120)
	deliverAll(f, openFile(t, f, strings.Repeat("x\n", 1<<18), "big.txt"))
	if f.activeView != filesViewContent {
		t.Fatal("the file is not shown")
	}
	l := tea.KeyPressMsg{Code: 'l', Text: "l"}
	first := f.Update(l)
	var later []tea.Cmd
	for range 39 {
		later = append(later, f.Update(l))
	}
	rendered := 0
	for _, cmd := range later {
		rendered += len(codeRenders(cmd))
	}
	if rendered != 0 {
		t.Fatalf("39 presses during a render started %d renders; want 0", rendered)
	}
	r := codeRenders(first)
	if len(r) != 1 {
		t.Fatalf("the first press rendered %d times; want 1", len(r))
	}
	next := codeRenders(f.Update(r[0]))
	if len(next) != 1 {
		t.Fatalf("%d renders followed the first; want 1", len(next))
	}
	if n := len(codeRenders(f.Update(next[0]))); n != 0 {
		t.Fatalf("the last render started %d more", n)
	}
}
