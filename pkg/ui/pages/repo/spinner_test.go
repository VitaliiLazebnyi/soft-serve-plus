package repo

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// The repository page turns one spinner: it stops once nothing loads, one
// tick chain starts again when a load begins, and a tick that was already
// handled is dropped.
func TestRepoSpinnerLifecycle(t *testing.T) {
	p := newPinPage(t)
	p.open()

	// Idle: a tick stops the spinner.
	if cmd := p.r.Update(p.r.spinner.Tick()); cmd != nil {
		t.Fatal("spinner keeps turning with nothing loading")
	}
	if p.r.ticking {
		t.Fatal("spinner marked as turning with nothing loading")
	}

	// A blame starts loading: exactly one tick chain starts.
	p.tab(1)
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	msgs := p.update(keyB)
	started := ticks(msgs)
	if len(started) != 1 {
		t.Fatalf("a load started %d tick chains; want 1", len(started))
	}
	// More messages while it loads start no other chain.
	if again := ticks(p.update(tea.KeyPressMsg{Code: 'x', Text: "x"})); len(again) != 0 {
		t.Fatalf("a key press while loading started %d more tick chains", len(again))
	}

	// Each tick turns the spinner once and schedules the next. The first
	// tick of a spinner carries no tag, which the spinner always accepts;
	// from the next one on, a tick delivered a second time is dropped.
	next := ticks(runCmd(p.r.Update(started[0])))
	if len(next) != 1 {
		t.Fatalf("a tick scheduled %d ticks; want 1", len(next))
	}
	tick := next[0].(spinner.TickMsg)
	before := p.view()
	if cmd := p.r.Update(tick); cmd == nil {
		t.Fatal("spinner stopped while loading")
	}
	turned := p.view()
	if turned == before {
		t.Fatal("tick did not turn the spinner")
	}
	if cmd := p.r.Update(tick); cmd != nil {
		t.Fatal("a duplicate tick started another chain")
	}
	if p.view() != turned {
		t.Fatal("a duplicate tick turned the spinner")
	}

	// The blame arrives: the next tick stops the spinner.
	p.settle(nonTicks(msgs)...)
	if cmd := p.r.Update(p.r.spinner.Tick()); cmd != nil {
		t.Fatal("spinner keeps turning after the load")
	}
}

// For its first waitBeforeLoading, a load in the Commits tab keeps the
// spinner turning but leaves the commit list on screen.
func TestLogLoadingWaitsBeforeShowingSpinner(t *testing.T) {
	p := newPinPage(t)
	p.open()

	waitBeforeLoading = time.Hour
	p.log.startLoading()
	if text, ok := p.log.Loading(); text != "" || !ok {
		t.Fatalf("Loading() before the wait = (%q, %v); want (\"\", true)", text, ok)
	}
	p.r.Update(selectTab(2))
	p.wantView("commits before the wait", "second-subject")
	p.notView("commits before the wait", "loading")

	waitBeforeLoading = 0
	if text, ok := p.log.Loading(); text != "loading commits…" || !ok {
		t.Fatalf("Loading() after the wait = (%q, %v); want (\"loading commits…\", true)", text, ok)
	}
	p.wantView("commits after the wait", "loading commits…")
}
