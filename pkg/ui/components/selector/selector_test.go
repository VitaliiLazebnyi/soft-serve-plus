package selector

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type testItem string

func (i testItem) ID() string          { return string(i) }
func (i testItem) Title() string       { return string(i) }
func (i testItem) Description() string { return "" }
func (i testItem) FilterValue() string { return string(i) }

type testDelegate struct{}

func (testDelegate) Height() int                         { return 1 }
func (testDelegate) Spacing() int                        { return 0 }
func (testDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (testDelegate) Render(w io.Writer, _ list.Model, _ int, item list.Item) {
	fmt.Fprint(w, item.(testItem).Title()) //nolint:errcheck
}

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

// Items are marked with zone IDs of their own, so an item named like another
// component's zone (e.g. a file named "footer") neither takes over that zone
// nor gets selected by clicks on it.
func TestItemZoneIgnoresSameNamedZone(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 10)
	t.Cleanup(c.Zone.Close)
	s := New(c, []IdentifiableItem{testItem("one"), testItem("footer")}, testDelegate{})
	c.Zone.Scan(lipgloss.JoinVertical(lipgloss.Left,
		s.View(),
		c.Zone.Mark("footer", "the footer"),
	))

	x, y := waitZone(t, c, "footer")
	s.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if got := s.Index(); got != 0 {
		t.Fatalf("clicking a zone named %q selected item %d", "footer", got)
	}

	x, y = waitZone(t, c, s.zonePrefix+"footer")
	s.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if got := s.Index(); got != 1 {
		t.Fatalf("clicking the footer item left item %d selected", got)
	}
}
