package repo

import (
	"errors"
	"testing"

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
