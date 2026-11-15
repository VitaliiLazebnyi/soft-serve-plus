package repo

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
)

// updateRendered delivers msg to m and then the renders its command
// produces, of a diff or of a code view, as the program would. It returns
// the command's other messages.
func updateRendered(m interface{ Update(tea.Msg) tea.Cmd }, msg tea.Msg) []tea.Msg {
	var rest []tea.Msg
	for _, out := range runCmd(m.Update(msg)) {
		switch out.(type) {
		case diffRenderedMsg, code.RenderedMsg:
			rest = append(rest, updateRendered(m, out)...)
		default:
			rest = append(rest, out)
		}
	}
	return rest
}
