package selection

import (
	"regexp"
	"strings"
	"testing"
)

// csi matches the CSI sequences the TUI itself emits (SGR styles and zone
// marks); anything else that controls the terminal must come from data.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// A project name and description any repository creator controls are shown
// to everyone on the repository list, so they must not carry escape
// sequences into the viewer's terminal.
func TestItemSanitisesProjectNameAndDescription(t *testing.T) {
	const escapes = "\x1bP1|x\x1b\\\x1b_Gx\x1b\\\x1b]52;c;Zm9v\x07\u009b2J"
	out := renderItem(t, fakeRepo{
		name:        "repo",
		projectName: "proj" + escapes,
		description: "desc" + escapes,
	}, 80)
	rest := csi.ReplaceAllString(out, "")
	for _, r := range rest {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("item holds control character %q: %q", r, out)
		}
	}
	if !strings.Contains(rest, "proj") || !strings.Contains(rest, "desc") {
		t.Errorf("item lost its text: %q", rest)
	}
}
