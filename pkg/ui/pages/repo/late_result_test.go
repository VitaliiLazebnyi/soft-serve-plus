package repo

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// A blame that arrives after the user switched tabs still reaches the Files
// tab, which then shows it and leaves it with esc rather than spinning for
// ever.
func TestBlameAfterTabSwitch(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)

	blame := nonTicks(p.update(keyB))
	p.settle(keyTab)
	if p.r.activeTab == 1 {
		t.Fatal("still on the Files tab")
	}
	p.settle(blame...)
	p.settle(keyShiftTab)
	if p.r.activeTab != 1 {
		t.Fatalf("tab = %d; want files", p.r.activeTab)
	}
	p.notView("blame after a tab switch", "loading")
	p.wantView("blame after a tab switch", "second-subject", "file-body-two")

	p.settle(keyEsc)
	p.wantView("esc after the blame", "README.md", "file.txt", "sub")
}

// A commit opened just before the user switched tabs still loads its diff in
// the Commits tab.
func TestCommitAfterTabSwitch(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(2)

	var sel tea.Msg
	for _, m := range p.update(keyEnter) {
		if _, ok := m.(selector.SelectMsg); ok {
			sel = m
		}
	}
	if sel == nil {
		t.Fatal("enter selected no commit")
	}
	load := nonTicks(p.update(sel))
	p.settle(keyTab)
	if p.r.activeTab == 2 {
		t.Fatal("still on the Commits tab")
	}
	p.settle(load...)
	p.settle(keyShiftTab)
	if p.r.activeTab != 2 {
		t.Fatalf("tab = %d; want commits", p.r.activeTab)
	}
	p.notView("commit after a tab switch", "loading")
	p.wantView("commit after a tab switch", "commit ", "second-subject", "file-body-two")

	p.settle(keyEsc)
	p.wantView("esc after the commit", "first-subject", "second-subject")
	p.notView("esc after the commit", "file-body-two")
}
