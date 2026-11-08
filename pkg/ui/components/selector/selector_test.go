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

func newTestSelector(t *testing.T, names ...string) *Selector {
	t.Helper()
	c := common.NewCommon(context.Background(), 40, 10)
	t.Cleanup(c.Zone.Close)
	items := make([]IdentifiableItem, len(names))
	for i, n := range names {
		items[i] = testItem(n)
	}
	s := New(c, items, testDelegate{})
	s.SetShowTitle(false)
	s.SetShowHelp(false)
	s.SetShowStatusBar(false)
	s.SetShowFilter(false)
	s.SetShowPagination(false)
	return s
}

// render draws the selector, as the program would, and waits until the zone
// of the item want has been recorded at row y.
func render(t *testing.T, s *Selector, want string, y int) {
	t.Helper()
	s.common.Zone.Scan(s.View())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if z := s.common.Zone.Get(s.zonePrefix + want); !z.IsZero() && z.StartY == y {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("zone %q was not drawn at row %d", want, y)
		}
		time.Sleep(time.Millisecond)
	}
}

// click sends a left click at row y and returns the messages its commands
// produce.
func click(s *Selector, y int) []tea.Msg {
	_, cmd := s.Update(tea.MouseClickMsg{X: 0, Y: y, Button: tea.MouseLeft})
	var msgs []tea.Msg
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				run(c)
			}
		default:
			msgs = append(msgs, msg)
		}
	}
	run(cmd)
	return msgs
}

func selected(msgs []tea.Msg) IdentifiableItem {
	for _, msg := range msgs {
		if msg, ok := msg.(SelectMsg); ok {
			return msg.IdentifiableItem
		}
	}
	return nil
}

// Clicking a row of a filtered list selects the item drawn on that row, and
// clicking it again opens it.
func TestClickFilteredItemSelectsIt(t *testing.T) {
	t.Parallel()
	s := newTestSelector(t, "alpha", "beta", "alpine", "gamma", "alps")
	s.SetFilterText("alp")
	if got := s.SelectedItem(); got == nil || got.ID() == "alpha" {
		t.Fatalf("filtered list starts at %v, want an item other than alpha", got)
	}
	row := -1
	for i, item := range s.VisibleItems() {
		if item.(testItem) == "alpha" {
			row = i
		}
	}
	if row < 1 {
		t.Fatalf("alpha is on row %d of the filtered list, want a later row", row)
	}
	render(t, s, "alpha", row)

	if got := selected(click(s, row)); got != nil {
		t.Fatalf("first click opened %q, want it only to select", got.ID())
	}
	if got := s.SelectedItem(); got == nil || got.ID() != "alpha" {
		t.Fatalf("click selected %v, want alpha", got)
	}

	render(t, s, "alpha", row)
	if got := selected(click(s, row)); got == nil || got.ID() != "alpha" {
		t.Fatalf("second click opened %v, want alpha", got)
	}
}

// Clicking the selected item of a filtered list opens it.
func TestClickSelectedFilteredItemOpensIt(t *testing.T) {
	t.Parallel()
	s := newTestSelector(t, "alpha", "beta", "gamma")
	s.SetFilterText("gam")
	render(t, s, "gamma", 0)

	if got := selected(click(s, 0)); got == nil || got.ID() != "gamma" {
		t.Fatalf("click opened %v, want gamma", got)
	}
}
