package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// newLogPage returns a repository page with only the Commits tab, over a
// repository whose one commit has a subject wider than any window used
// here, open on the commit's diff.
func newLogPage(t *testing.T) *pinPage {
	t.Helper()
	repo := newDiskRepo(t, "r", "main", "# r\n")
	gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", strings.Repeat("x", 300))

	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	p := &pinPage{t: t, repo: repo, log: NewLog(c)}
	p.r = New(c, p.log)
	p.r.SetSize(120, 40)
	p.open()
	p.settle(keyEnter)
	if p.log.activeView != logViewDiff {
		t.Fatalf("after enter, view = %v; want the diff", p.log.activeView)
	}
	return p
}

// A resize reaches the repository page after its parent sized it, less the
// parent's margins. Neither the page nor its tabs size themselves again from
// the window size in the message: the commit page is loaded once, by the
// size the list has, and the diff is wrapped to the tab's width.
func TestResizeKeepsParentSize(t *testing.T) {
	t.Parallel()
	p := newLogPage(t)

	// The parent takes 4 columns and 6 rows for itself. Like the top
	// models, it sizes the page before and after each message.
	p.r.SetSize(100, 30)
	perPage := p.log.selector.PerPage()
	queue := p.update(tea.WindowSizeMsg{Width: 104, Height: 36})
	if w := p.log.width; w != 100 {
		t.Errorf("after the resize the Commits tab is %d columns wide; want the 100 the parent gave", w)
	}
	p.r.SetSize(100, 30)

	var loads []LogItemsMsg
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 10000 {
			t.Fatal("page never settles")
		}
		var msg tea.Msg
		msg, queue = queue[0], queue[1:]
		switch msg := msg.(type) {
		case LogItemsMsg:
			loads = append(loads, msg)
		case common.ErrorMsg:
			t.Fatalf("page reported an error: %v", msg.Err)
		}
		queue = append(queue, nonTicks(p.update(msg))...)
		p.r.SetSize(100, 30)
	}
	if len(loads) != 1 {
		t.Errorf("a resize loaded the commit page %d times; want once", len(loads))
	}
	if len(loads) > 0 && loads[0].limit != perPage {
		t.Errorf("the page was loaded %d commits at a time; want the %d the list shows", loads[0].limit, perPage)
	}

	if p.log.activeView != logViewDiff {
		t.Fatalf("after the resize, view = %v; want the diff", p.log.activeView)
	}
	widest := 0
	for _, line := range strings.Split(p.log.vp.GetContent(), "\n") {
		widest = max(widest, lipgloss.Width(line))
	}
	if widest > 100 {
		t.Errorf("the diff is %d columns wide after the resize; want at most the tab's 100", widest)
	}
}

// soft browse hides the clone command. The blank header where it would be
// is no click zone, and a click on it copies nothing: copying "" would clear
// the clipboard. With the command shown, a click on it copies it.
func TestHeaderClickWithoutCloneCmd(t *testing.T) {
	t.Parallel()
	for _, hide := range []bool{true, false} {
		ctx := context.Background()
		c := uitest.NewCommon(t, ctx, nil, nil)
		t.Cleanup(c.Zone.Close)
		c.HideCloneCmd = hide
		r := New(c, NewReadme(c))
		r.SetSize(80, 24)
		r.Update(RepoMsg(fakeRepo{}))
		r.state = readyState

		// Zones are stored in the order they are marked, so once the
		// sentinel after the page is stored, so is every zone of the page.
		c.Zone.Scan(r.View() + c.Zone.Mark("sentinel", "x"))
		deadline := time.Now().Add(time.Minute)
		for c.Zone.Get("sentinel").IsZero() {
			if time.Now().After(deadline) {
				t.Fatal("the zones were never stored")
			}
			time.Sleep(time.Millisecond)
		}

		var copied []CopyMsg
		header := lipgloss.Height(r.headerView())
		for y := range header {
			for x := range 80 {
				for _, msg := range runCmd(r.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})) {
					if m, ok := msg.(CopyMsg); ok {
						copied = append(copied, m)
					}
				}
			}
		}
		switch {
		case hide && len(copied) != 0:
			t.Fatalf("with the clone command hidden, clicks on the header copied %q", copied[0].Text)
		case hide && !c.Zone.Get(r.zonePrefix+"url").IsZero():
			t.Fatal("with the clone command hidden, the header marks a zone for it")
		case !hide && (len(copied) == 0 || !strings.HasPrefix(copied[0].Text, "git clone ")):
			t.Fatalf("with the clone command shown, clicks on the header copied %v; want the command", copied)
		}
	}
}

// A diff rendered again on a resize is wrapped to the width the parent gave
// the tab, not to the window's, in the Commits tab and in the Stash tab.
func TestDiffResizeKeepsParentWidth(t *testing.T) {
	t.Parallel()
	for name, p := range newDiffPanes(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			diff := &git.Diff{Patch: "+" + strings.Repeat("y", 300) + "\n"}
			updateRendered(p.m, findMsg[diffRenderedMsg](t, runCmd(p.open(diff))))
			if !p.shown() {
				t.Fatal("the diff is not shown")
			}

			// The parent takes 4 columns for itself.
			p.m.SetSize(100, 40)
			for _, r := range renders(p.m.Update(tea.WindowSizeMsg{Width: 104, Height: 40})) {
				updateRendered(p.m, r)
			}
			widest := 0
			for _, line := range strings.Split(p.vp.GetContent(), "\n") {
				widest = max(widest, lipgloss.Width(line))
			}
			if widest > 100 {
				t.Fatalf("the diff is %d columns wide after the resize; want at most the tab's 100", widest)
			}
		})
	}
}

// Every tab of the repository page, and its status bar, keeps the size the
// page gave it when the window size message reaches it.
func TestResizeKeepsEveryTabsSize(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	// The parent takes 4 columns and 6 rows for itself.
	p.r.SetSize(100, 30)
	type size struct{ w, h int }
	tabs := map[string]func() size{
		"Readme":   func() size { return size{p.readme.width, p.readme.height} },
		"Files":    func() size { return size{p.files.width, p.files.height} },
		"Commits":  func() size { return size{p.log.width, p.log.height} },
		"Stash":    func() size { return size{p.stash.width, p.stash.height} },
		"Branches": func() size { return size{p.branches.width, p.branches.height} },
		"Tags":     func() size { return size{p.tags.width, p.tags.height} },
	}
	want := map[string]size{}
	for name, tabSize := range tabs {
		if w := tabSize().w; w != 100 {
			t.Fatalf("%s tab is %d columns wide; want the page's 100", name, w)
		}
		want[name] = tabSize()
	}
	p.r.Update(tea.WindowSizeMsg{Width: 104, Height: 36})
	for name, tabSize := range tabs {
		if got := tabSize(); got != want[name] {
			t.Errorf("%s tab is %dx%d after the resize; want the %dx%d the page gave it",
				name, got.w, got.h, want[name].w, want[name].h)
		}
	}
	if w := lipgloss.Width(p.r.statusbar.View()); w != 100 {
		t.Errorf("the status bar is %d columns wide after the resize; want the page's 100", w)
	}
}
