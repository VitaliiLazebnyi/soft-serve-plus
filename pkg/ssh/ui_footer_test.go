package ssh

import (
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
)

// Toggling the help footer on the repository page resizes the page before
// the page handles the toggle, so the Commits tab loads its page once, by the
// height it has with the footer, rather than by the old height and then
// again.
func TestUIFooterToggleLoadsCommitPageOnce(t *testing.T) {
	ui := newRepoTestUI(t)
	commitTo(t, ui, "a")

	// send delivers msg and every message the commands produce, and returns
	// how many commit pages were delivered.
	send := func(msg tea.Msg) int {
		t.Helper()
		loads := 0
		queue := []tea.Msg{msg}
		for steps := 0; len(queue) > 0; steps++ {
			if steps > 10000 {
				t.Fatal("UI never settles")
			}
			msg, queue = queue[0], queue[1:]
			if _, ok := msg.(spinner.TickMsg); ok || msg == nil {
				continue
			}
			if _, ok := msg.(repo.LogItemsMsg); ok {
				loads++
			}
			_, cmd := ui.Update(msg)
			queue = append(queue, runCmd(cmd)...)
		}
		return loads
	}

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	send(ui.openRepoCmd("a")())
	send(tabs.SelectTabMsg(2))

	help := tea.KeyPressMsg{Code: '?', Text: "?"}
	for i, state := range []string{"shown", "hidden"} {
		if n := send(help); n != 1 {
			t.Fatalf("toggle %d, help %s: the commit page was loaded %d times; want once", i+1, state, n)
		}
	}
}
