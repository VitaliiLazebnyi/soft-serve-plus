package repo

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// An error from a load started for a repository or ref the user has left is
// dropped like any late result: it neither reaches a pane nor the error
// screen.
func TestRepoDropsStaleError(t *testing.T) {
	p := newPinPage(t)
	p.open()
	stale := errorMsg{err: errors.New("late"), to: paneLog, gen: p.r.gen - 1}
	for _, m := range p.update(stale) {
		if _, ok := m.(common.ErrorMsg); ok {
			t.Fatal("a stale error reached the error screen")
		}
	}

	current := errorMsg{err: errors.New("now"), to: paneLog, gen: p.r.gen}
	var reported bool
	for _, m := range p.update(current) {
		if e, ok := m.(common.ErrorMsg); ok && e.Err == current.err {
			reported = true
		}
	}
	if !reported {
		t.Fatal("a current error did not reach the error screen")
	}
}

// A list that fails to load is not left showing what was loaded before,
// perhaps for another ref or repository.
func TestListErrorClearsPreviousItems(t *testing.T) {
	p := newPinPage(t)
	p.open()
	fail := func(to paneID) {
		t.Helper()
		p.update(errorMsg{err: errors.New("listing failed"), to: to, gen: p.r.gen})
	}

	// Log: a page reload fails.
	p.log.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	fail(paneLog)
	if n := len(p.log.selector.VisibleItems()); n != 0 || p.log.activeView != logViewCommits {
		t.Errorf("Commits tab shows %d commits in view %v after its load failed; want none", n, p.log.activeView)
	}

	// Stash: the list reloads and fails.
	p.stash.Init()
	fail(paneStash)
	if n := len(p.stash.list.Items()); n != 0 || p.stash.state != stashStateList {
		t.Errorf("Stash tab shows %d stashes in state %v after its load failed; want none", n, p.stash.state)
	}

	// Branches: the listing fails.
	p.branches.Init()
	fail(paneBranches)
	if n := len(p.branches.selector.Items()); n != 0 || p.branches.StatusBarValue() != "" {
		t.Errorf("Branches tab shows %d branches after its load failed; want none", n)
	}
}
