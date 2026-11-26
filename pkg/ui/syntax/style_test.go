package syntax

import (
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ui/uitest/uiwait"
)

// A comment is rendered in the same colour every time. glamour's comment
// colour lay exactly between two 256-colour palette entries, and chroma took
// either at random, about one render in four the other one. 200 renders all
// alike rule that out.
func TestCommentColourIsStable(t *testing.T) {
	waitSlots(t)
	const want = "  \x1b[38;5;241m// c\x1b[0m\x1b[38;5;251m\x1b[0m\n"
	for i := range 200 {
		got, ok := Highlight(StyleConfig(), "main.go", "// c\n", false, uiwait.Unbounded)
		if !ok || got != want {
			t.Fatalf("render %d: Highlight = %q, %v; want %q, true", i, got, ok, want)
		}
	}
}
