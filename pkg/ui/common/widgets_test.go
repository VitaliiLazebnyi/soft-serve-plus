package common_test

import (
	"context"
	"slices"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// TruncateString measures like lipgloss, so emoji with a variation selector
// or a skin-tone modifier never make a truncated line wider than asked.
func TestTruncateStringMeasuresLikeLipgloss(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{in: "hello", width: 10, want: "hello"},
		{in: "hello world", width: 6, want: "hello…"},
		{in: "❤️abc", width: 4, want: "❤️a…"},
		{in: "👍🏽ab", width: 4, want: "👍🏽ab"},
		{in: "👍🏽abc", width: 4, want: "👍🏽a…"},
		{in: "🔒 repo", width: 3, want: "🔒…"},
		{in: "anything", width: -1, want: ""},
	} {
		got := common.TruncateString(tc.in, tc.width)
		if got != tc.want {
			t.Errorf("TruncateString(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
		if w := lipgloss.Width(got); tc.width >= 0 && w > tc.width {
			t.Errorf("TruncateString(%q, %d) is %d cells wide", tc.in, tc.width, w)
		}
	}
	if got := common.TruncateWidth("❤️abc", 3); got != "❤️a" {
		t.Errorf("TruncateWidth = %q, want %q", got, "❤️a")
	}
}

// CopyKey describes what the copy key copies without changing the shared
// key map other panes read.
func TestCopyKey(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 24)
	before := c.KeyMap.Copy.Help().Desc
	k := c.CopyKey("copy hash")
	if got := k.Help().Desc; got != "copy hash" {
		t.Errorf("help = %q, want copy hash", got)
	}
	if !slices.Equal(k.Keys(), c.KeyMap.Copy.Keys()) {
		t.Errorf("keys = %v, want %v", k.Keys(), c.KeyMap.Copy.Keys())
	}
	if got := c.KeyMap.Copy.Help().Desc; got != before {
		t.Errorf("shared copy key help changed to %q", got)
	}
}
