package repo

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestWrapTextMeasuresLikeLipgloss checks that commit text wraps at the width
// lipgloss measures, so wide characters such as emoji never overflow the pane.
func TestWrapTextMeasuresLikeLipgloss(t *testing.T) {
	const width = 9
	in := "fix: 🎉🎉🎉 ship it 🚀🚀🚀🚀 now\nsecond\tline 👍🏽👍🏽👍🏽👍🏽"
	out := wrapText(in, width)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("line %q is %d columns wide, want at most %d", line, w, width)
		}
	}
	if strings.Contains(out, "\t") {
		t.Errorf("tab not expanded: %q", out)
	}
	if got, want := strings.ReplaceAll(strings.ReplaceAll(out, "\n", ""), " ", ""),
		strings.NewReplacer("\n", "", " ", "", "\t", "").Replace(in); got != want {
		t.Errorf("wrapping lost text: got %q, want %q", got, want)
	}
	if got := wrapText("unchanged", 0); got != "unchanged" {
		t.Errorf("width 0: got %q, want input unchanged", got)
	}
}
