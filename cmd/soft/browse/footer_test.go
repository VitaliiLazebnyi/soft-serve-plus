package browse

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
)

// Toggling the help footer resizes the repository page before the page
// handles the toggle, so the Commits tab loads its page once, by the height
// it has with the footer, rather than by the old height and then again.
func TestBrowseFooterToggleLoadsCommitPageOnce(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	m := newModel(ctx, newDriveRepo(t))
	t.Cleanup(m.common.Zone.Close)

	// send delivers msg and every message the commands produce, and
	// returns how many commit pages were delivered.
	send := func(msg tea.Msg) int {
		t.Helper()
		loads := 0
		queue := []tea.Msg{msg}
		for steps := 0; len(queue) > 0; steps++ {
			if steps > 10000 {
				t.Fatal("model never settles")
			}
			msg, queue = queue[0], queue[1:]
			if _, ok := msg.(spinner.TickMsg); ok || msg == nil {
				continue
			}
			if _, ok := msg.(repo.LogItemsMsg); ok {
				loads++
			}
			_, cmd := m.Update(msg)
			queue = append(queue, msgs(t, cmd)...)
		}
		return loads
	}

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, msg := range msgs(t, m.Init()) {
		send(msg)
	}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	send(tab)
	send(tab)

	help := tea.KeyPressMsg{Code: '?', Text: "?"}
	for i, state := range []string{"shown", "hidden"} {
		if n := send(help); n != 1 {
			t.Fatalf("toggle %d, help %s: the commit page was loaded %d times; want once", i+1, state, n)
		}
	}
}
