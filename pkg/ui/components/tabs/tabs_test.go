package tabs

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// waitZone waits for the zone manager to store the zone with the given ID,
// since Scan hands zones to a background worker.
func waitZone(t *testing.T, c common.Common, id string) (x, y int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if z := c.Zone.Get(id); z != nil && !z.IsZero() {
			return z.StartX, z.StartY
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("zone %q was never stored", id)
	return 0, 0
}

// A zone another component marks with a tab's name, such as a repository or
// file named "Files", must not switch to that tab when clicked.
func TestTabZoneIgnoresSameNamedZone(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	tb := New(c, []string{"About", "Files"})
	c.Zone.Scan(lipgloss.JoinVertical(lipgloss.Left,
		tb.View(),
		c.Zone.Mark("Files", "a repository named Files"),
	))

	x, y := waitZone(t, c, "Files")
	tb.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if tb.activeTab != 0 {
		t.Fatalf("clicking a zone named %q switched to tab %d", "Files", tb.activeTab)
	}

	x, y = waitZone(t, c, tb.zonePrefix+"Files")
	tb.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if tb.activeTab != 1 {
		t.Fatalf("clicking the Files tab left tab %d active", tb.activeTab)
	}
}
