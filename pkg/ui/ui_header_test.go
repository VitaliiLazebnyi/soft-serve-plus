package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// The selection page starts with the server name, trimmed.
func TestUISelectionShowsServerName(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Name = "  Test Server  "
	ui := New(uitest.NewCommon(t, t.Context(), cfg, nil), 80, 24, "")
	ui.Init()

	var first string
	for _, line := range strings.Split(ansi.Strip(ui.View().Content), "\n") {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	// The style pads the name by one cell on each side and indents it by
	// one more, inside the app's two-cell margin.
	if want := "    Test Server "; strings.TrimRight(first, " ") != strings.TrimRight(want, " ") {
		t.Fatalf("first line = %q; want the server name %q", first, want)
	}
}
