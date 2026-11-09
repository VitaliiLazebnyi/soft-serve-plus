package selector

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type testItem string

func (i testItem) ID() string          { return string(i) }
func (i testItem) Title() string       { return string(i) }
func (i testItem) Description() string { return "" }
func (i testItem) FilterValue() string { return string(i) }

// runCmd runs cmd and every command it batches, and returns the messages
// they produce.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, runCmd(c)...)
		}
		return msgs
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// bubbletea runs every command in its own goroutine while Update keeps
// changing the list. The select command must carry the item that was selected
// when Update returned, and must not read the list, which raced with Update.
func TestSelectItemCmdDoesNotReadModel(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(t.Context(), 80, 40)
	items := []IdentifiableItem{testItem("a"), testItem("b")}
	s := New(c, items, list.NewDefaultDelegate())

	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter returned no command")
	}

	var (
		wg   sync.WaitGroup
		msgs []tea.Msg
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		msgs = runCmd(cmd)
	}()
	for range 100 {
		s.SetItems(nil)
		s.Select(0)
		s.SetItems([]IdentifiableItem{testItem("c")})
	}
	wg.Wait()

	var got []SelectMsg
	for _, msg := range msgs {
		if m, ok := msg.(SelectMsg); ok {
			got = append(got, m)
		}
	}
	if len(got) != 1 || got[0].IdentifiableItem != testItem("a") {
		t.Fatalf("select messages = %#v, want one for item a", got)
	}
}

type testDelegate struct{ common *common.Common }

func (d testDelegate) Height() int                         { return 1 }
func (d testDelegate) Spacing() int                        { return 0 }
func (d testDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d testDelegate) Render(w io.Writer, _ list.Model, _ int, item list.Item) {
	i := item.(testItem)
	fmt.Fprint(w, d.common.Zone.Mark(i.ID(), i.Title())) //nolint:errcheck
}

func newTestSelector(t *testing.T, names ...string) *Selector {
	t.Helper()
	c := common.NewCommon(context.Background(), 40, 10)
	t.Cleanup(c.Zone.Close)
	items := make([]IdentifiableItem, len(names))
	for i, n := range names {
		items[i] = testItem(n)
	}
	s := New(c, items, testDelegate{&c})
	s.SetShowTitle(false)
	s.SetShowHelp(false)
	s.SetShowStatusBar(false)
	s.SetShowFilter(false)
	s.SetShowPagination(false)
	return s
}

// render draws the selector, as the program would, and waits until the zone
// of want has been recorded at row y.
func render(t *testing.T, s *Selector, want string, y int) {
	t.Helper()
	s.common.Zone.Scan(s.View())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if z := s.common.Zone.Get(want); !z.IsZero() && z.StartY == y {
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

// "?" typed while filtering goes into the filter.
func TestSelectorTypesHelpKeyWhileFiltering(t *testing.T) {
	t.Parallel()
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	s := New(c, []IdentifiableItem{testItem("a?"), testItem("b")}, testDelegate{&c})
	for _, k := range []tea.KeyPressMsg{
		{Code: '/', Text: "/"},
		{Code: 'a', Text: "a"},
		{Code: '?', Text: "?"},
	} {
		s.Update(k)
	}
	if s.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v, want filtering", s.FilterState())
	}
	if got := s.FilterValue(); got != "a?" {
		t.Errorf("filter = %q, want %q", got, "a?")
	}
}
