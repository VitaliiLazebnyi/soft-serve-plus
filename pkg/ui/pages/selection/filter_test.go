package selection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// cursorPkg is the package of the cursor blink messages.
var cursorPkg = reflect.TypeOf(cursor.BlinkMsg{}).PkgPath()

// filterDriver feeds keys to a selection page and the messages their
// commands produce back to it, the way the bubbletea runtime does. It
// collects the SelectMsg the page sends, which the UI above it handles.
type filterDriver struct {
	t       *testing.T
	s       *Selection
	selects []selector.SelectMsg
}

// run runs cmd to completion, however slow the machine, and returns its
// messages. The test turns off the filter input's cursor blinking, whose
// every blink waits half a second; the cursor messages left are dropped
// instead of delivered, as delivering one could schedule another.
func (d *filterDriver) run(cmd tea.Cmd) []tea.Msg {
	d.t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(uitest.HangGuard):
		d.t.Fatal("command did not return")
	}
	if msg != nil && reflect.TypeOf(msg).PkgPath() == cursorPkg {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, d.run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// noBlink turns off the blinking of the filter input's cursor, so that a
// test does not wait half a second for each blink.
func noBlink(s *Selection) {
	st := s.selector.FilterInput.Styles()
	st.Cursor.Blink = false
	s.selector.FilterInput.SetStyles(st)
}

func (d *filterDriver) send(msg tea.Msg) {
	d.t.Helper()
	queue := []tea.Msg{msg}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			d.t.Fatal("page never settles")
		}
		msg, queue = queue[0], queue[1:]
		if sel, ok := msg.(selector.SelectMsg); ok {
			d.selects = append(d.selects, sel)
		}
		queue = append(queue, d.run(d.s.Update(msg))...)
	}
}

func (d *filterDriver) want(step string, state list.FilterState, selected string, selects int) {
	d.t.Helper()
	if got := d.s.selector.FilterState(); got != state {
		d.t.Fatalf("%s: filter state = %v; want %v", step, got, state)
	}
	if it := d.s.selector.SelectedItem(); it == nil || it.ID() != selected {
		d.t.Fatalf("%s: highlighted %v; want %q", step, it, selected)
	}
	if len(d.selects) != selects {
		d.t.Fatalf("%s: %d SelectMsg; want %d", step, len(d.selects), selects)
	}
}

// Filtering the repository list: "/" starts the filter, typed keys go to it,
// the first enter applies it, the second opens the highlighted repository,
// and esc clears the filter.
func TestSelectionFilterFlow(t *testing.T) {
	c := common.NewCommon(context.Background(), 80, 40)
	t.Cleanup(c.Zone.Close)
	s := New(c)
	s.SetSize(80, 40)
	its := make([]selector.IdentifiableItem, 0, 3)
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		its = append(its, NewItem(c, fakeRepo{name: n}))
	}
	s.setItems(its)
	noBlink(s)
	d := &filterDriver{t: t, s: s}
	key := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

	d.want("start", list.Unfiltered, "alpha", 0)

	d.send(key('/'))
	d.want("after /", list.Filtering, "alpha", 0)
	if !s.IsFiltering() {
		t.Fatal("page does not report filtering")
	}

	d.send(key('b'))
	d.send(key('r'))
	d.want("after typing", list.Filtering, "bravo", 0)
	if n := len(s.selector.VisibleItems()); n != 1 {
		t.Fatalf("%d repositories match %q; want 1", n, "br")
	}

	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	d.send(enter)
	d.want("after the first enter", list.FilterApplied, "bravo", 0)

	d.send(enter)
	d.want("after the second enter", list.FilterApplied, "bravo", 1)
	if got := d.selects[0].ID(); got != "bravo" {
		t.Fatalf("SelectMsg for %q; want bravo", got)
	}

	d.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := s.selector.FilterState(); got != list.Unfiltered {
		t.Fatalf("after esc: filter state = %v; want unfiltered", got)
	}
	if n := len(s.selector.VisibleItems()); n != 3 {
		t.Fatalf("after esc: %d repositories listed; want 3", n)
	}
	if len(d.selects) != 1 {
		t.Fatalf("after esc: %d SelectMsg; want 1", len(d.selects))
	}
}
