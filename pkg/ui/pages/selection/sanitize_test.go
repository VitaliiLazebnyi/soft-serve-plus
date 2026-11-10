package selection

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// escapes holds the string sequences the bubbletea renderer passes through
// to the terminal: a DCS, an APC, an OSC 8 hyperlink with a hidden target, an
// OSC 52 clipboard write and a C1 CSI.
const escapes = "\x1bP1|x\x1b\\" +
	"\x1b_Gx\x1b\\" +
	"\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" +
	"\x1b]52;c;Zm9v\x07" +
	"\u009b2J"

// csi matches the CSI sequences the TUI itself emits (SGR styles and zone
// marks); anything else that controls the terminal must come from data.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// assertClean fails if out, less the TUI's own CSI sequences, holds a
// control character other than newline and tab, or lacks any of want.
func assertClean(t *testing.T, out string, want ...string) {
	t.Helper()
	rest := csi.ReplaceAllString(out, "")
	for _, r := range rest {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("output holds control character %q: %q", r, out)
		}
	}
	for _, w := range want {
		if !strings.Contains(rest, w) {
			t.Errorf("output lacks %q: %q", w, rest)
		}
	}
}

// A project name and description any repository creator controls are shown
// to everyone on the repository list, so they must not carry escape
// sequences into the viewer's terminal.
func TestItemSanitisesProjectNameAndDescription(t *testing.T) {
	out := renderItem(t, fakeRepo{
		name:        "repo",
		projectName: "proj" + escapes,
		description: "desc" + escapes,
	}, 80)
	assertClean(t, out, "proj", "desc")
}

// The server README is shown to everyone who connects, and glamour keeps
// string sequences in it.
func TestReadmePaneSanitisesReadme(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)
	s := New(c)
	s.SetSize(120, 40)
	s.setReadme("# Welcome"+escapes+"\n\nreadme-text"+escapes+"\n", "README.md")
	s.activePane = readmePane
	assertClean(t, s.View(), "Welcome", "readme-text")
}
