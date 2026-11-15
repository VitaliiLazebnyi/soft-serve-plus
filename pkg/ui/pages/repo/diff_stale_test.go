package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// A render that finishes after a newer one started, or after the user left
// the diff, is dropped.
func TestDiffRenderDropsStaleResults(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	l := NewLog(common.NewCommon(ctx, 120, 40))
	l.SetSize(120, 40)
	l.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	sig := &gitm.Signature{Name: "a", Email: "a@b", When: time.Now()}
	commit := &git.Commit{
		ID:      gitm.MustIDFromString("0123456789abcdef0123456789abcdef01234567"),
		Author:  sig,
		Message: "subject\n",
	}
	l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: commit}})
	diff := &git.Diff{Patch: "+" + strings.Repeat("y", 200) + "\n"}
	updateRendered(l, LogDiffMsg{commit: commit, diff: diff, gen: l.gen})
	if l.activeView != logViewDiff {
		t.Fatalf("view = %v; want the diff", l.activeView)
	}

	widest := func() int {
		w := 0
		for _, line := range strings.Split(l.vp.GetContent(), "\n") {
			w = max(w, lipgloss.Width(line))
		}
		return w
	}
	wide := findMsg[diffRenderedMsg](t, runCmd(l.Update(tea.WindowSizeMsg{Width: 100, Height: 40})))
	narrow := findMsg[diffRenderedMsg](t, runCmd(l.Update(tea.WindowSizeMsg{Width: 60, Height: 40})))
	l.Update(narrow)
	l.Update(wide)
	if w := widest(); w > 60 {
		t.Fatalf("a stale render for a wider window replaced the current one: %d columns", w)
	}

	// Leaving the diff drops a render in flight.
	late := findMsg[diffRenderedMsg](t, runCmd(l.Update(tea.WindowSizeMsg{Width: 80, Height: 40})))
	l.Update(GoBackMsg{})
	l.Update(late)
	if l.activeView != logViewCommits || l.currentDiff != nil {
		t.Fatalf("after going back, view = %v and diff kept = %v; want the commits and no diff", l.activeView, l.currentDiff != nil)
	}
}
