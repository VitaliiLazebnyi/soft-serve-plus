package repo

import tea "charm.land/bubbletea/v2"

// updateRendered delivers msg to m and then the diff renders its command
// produces, as the program would. It returns the command's other messages.
func updateRendered(m interface{ Update(tea.Msg) tea.Cmd }, msg tea.Msg) []tea.Msg {
	var rest []tea.Msg
	for _, out := range runCmd(m.Update(msg)) {
		if r, ok := out.(diffRenderedMsg); ok {
			m.Update(r)
			continue
		}
		rest = append(rest, out)
	}
	return rest
}
