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
	"github.com/charmbracelet/soft-serve/pkg/ui/components/viewport"
)

// diffPane is a Log or a Stash pane showing a diff, for the render tests.
type diffPane struct {
	m  interface{ Update(tea.Msg) tea.Cmd }
	vp *viewport.Viewport
	// shown reports whether the pane shows the diff.
	shown func() bool
	// open shows a diff and returns the command of its first render.
	open func(*git.Diff) tea.Cmd
}

func newDiffPanes(t *testing.T) map[string]diffPane {
	t.Helper()
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

	s := NewStash(common.NewCommon(ctx, 120, 40))
	s.SetSize(120, 40)
	s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.Update(StashListMsg{stashes: []*gitm.Stash{{Message: "stash"}}})

	return map[string]diffPane{
		"log": {
			m: l, vp: l.vp,
			shown: func() bool { return l.activeView == logViewDiff && l.currentDiff != nil },
			open: func(d *git.Diff) tea.Cmd {
				l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: commit}})
				return l.Update(LogDiffMsg{commit: commit, diff: d, gen: l.gen})
			},
		},
		"stash": {
			m: s, vp: s.vp,
			shown: func() bool { return s.state == stashStatePatch && s.currentPatch.Diff != nil },
			open: func(d *git.Diff) tea.Cmd {
				return s.Update(StashPatchMsg{Diff: d, gen: s.gen})
			},
		},
	}
}

// renders runs cmd and returns the renders it produces.
func renders(cmd tea.Cmd) []diffRenderedMsg {
	var out []diffRenderedMsg
	for _, m := range runCmd(cmd) {
		if r, ok := m.(diffRenderedMsg); ok {
			out = append(out, r)
		}
	}
	return out
}

// A render that finishes after the width changed, or after the user left
// the diff, is dropped, and the current width is rendered once.
func TestDiffRenderDropsStaleResults(t *testing.T) {
	for name, p := range newDiffPanes(t) {
		t.Run(name, func(t *testing.T) {
			diff := &git.Diff{Patch: "+" + strings.Repeat("y", 200) + "\n"}
			updateRendered(p.m, findMsg[diffRenderedMsg](t, runCmd(p.open(diff))))
			if !p.shown() {
				t.Fatal("the diff is not shown")
			}
			widest := func() int {
				w := 0
				for _, line := range strings.Split(p.vp.GetContent(), "\n") {
					w = max(w, lipgloss.Width(line))
				}
				return w
			}

			// A render for 100 columns is in flight when the window
			// narrows to 60: it is dropped, and 60 is rendered.
			wide := renders(p.m.Update(tea.WindowSizeMsg{Width: 100, Height: 40}))
			if len(wide) != 1 {
				t.Fatalf("a width change started %d renders; want 1", len(wide))
			}
			if n := len(renders(p.m.Update(tea.WindowSizeMsg{Width: 60, Height: 40}))); n != 0 {
				t.Fatalf("a width change started %d renders while one was in flight; want 0", n)
			}
			narrow := renders(p.m.Update(wide[0]))
			if len(narrow) != 1 {
				t.Fatalf("the stale render was followed by %d renders; want 1", len(narrow))
			}
			if w := widest(); w <= 100 {
				t.Fatalf("the stale render for 100 columns was shown: %d columns wide", w)
			}
			updateRendered(p.m, narrow[0])
			if w := widest(); w > 60 {
				t.Fatalf("the current render is %d columns wide; want at most 60", w)
			}

			// Leaving the diff drops a render in flight and starts none.
			late := renders(p.m.Update(tea.WindowSizeMsg{Width: 80, Height: 40}))
			if len(late) != 1 {
				t.Fatalf("a width change started %d renders; want 1", len(late))
			}
			p.m.Update(GoBackMsg{})
			if n := len(renders(p.m.Update(late[0]))); n != 0 {
				t.Fatalf("a render after going back started %d more", n)
			}
			if p.shown() || p.vp.GetContent() != "" {
				t.Fatal("the diff is still shown, or held, after going back")
			}
		})
	}
}

// A render whose result never comes back, because the user left the page
// while it ran, does not keep the next diff from rendering once the pane
// is reset.
func TestDiffRenderLostResult(t *testing.T) {
	for name, p := range newDiffPanes(t) {
		t.Run(name, func(t *testing.T) {
			updateRendered(p.m, findMsg[diffRenderedMsg](t, runCmd(p.open(&git.Diff{Patch: "+one\n"}))))
			// A width change starts a render; its result is lost.
			if n := len(renders(p.m.Update(tea.WindowSizeMsg{Width: 90, Height: 40}))); n != 1 {
				t.Fatalf("a width change started %d renders; want 1", n)
			}
			p.m.Update(GoBackMsg{})
			// The repository is opened again, and a diff with it.
			p.m.Update(RepoMsg(fakeRepo{}))
			r := renders(p.open(&git.Diff{Patch: "+two\n"}))
			if len(r) != 1 {
				t.Fatalf("opening a diff after a lost render started %d renders; want 1", len(r))
			}
			p.m.Update(r[0])
			if !p.shown() || !strings.Contains(p.vp.GetContent(), "two") {
				t.Fatal("the diff is not shown")
			}
		})
	}
}

// Width changes while a render is in flight start no render of their own:
// a client that resizes over and over costs at most two renders, the one in
// flight and one for the width it ends at.
func TestDiffRenderOneInFlight(t *testing.T) {
	for name, p := range newDiffPanes(t) {
		t.Run(name, func(t *testing.T) {
			first := p.open(&git.Diff{Patch: "+line\n"})
			total := 0
			for i := range 64 {
				total += len(renders(p.m.Update(tea.WindowSizeMsg{Width: 80 + i%2, Height: 40})))
			}
			if total != 0 {
				t.Fatalf("64 width changes during a render started %d renders; want 0", total)
			}
			r := renders(first)
			if len(r) != 1 {
				t.Fatalf("the first render produced %d results; want 1", len(r))
			}
			next := renders(p.m.Update(r[0]))
			if len(next) != 1 {
				t.Fatalf("after the first render %d renders started; want 1", len(next))
			}
			if n := len(renders(p.m.Update(next[0]))); n != 0 {
				t.Fatalf("the last render started %d more; want none", n)
			}
			if !p.shown() {
				t.Fatal("the diff is not shown")
			}
		})
	}
}
