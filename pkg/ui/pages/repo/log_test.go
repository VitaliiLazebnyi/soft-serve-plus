package repo

import (
	"context"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// Toggling the footer resizes the commit list only after the Log pane has
// built its load, so a load paged by the old size is loaded again by the size
// the list has when it arrives, or the page shown after the resize has rows
// that were never loaded.
func TestLogReloadsPageLoadedForAnotherSize(t *testing.T) {
	repo := newDiskRepo(t, "r", "main", "# r\n")
	for range 40 {
		gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", "c")
	}

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	l := NewLog(c)
	l.SetSize(80, 40)
	l.Update(RepoMsg(repo))
	ref := updateRefCmd(t.Context(), repo, 1)()
	refMsg, ok := ref.(RefMsg)
	if !ok {
		t.Fatalf("updateRefCmd = %T %v; want a RefMsg", ref, ref)
	}
	l.Update(refMsg)
	l.Update(l.countCommitsCmd()())

	l.Update(l.updateCommitsCmd()())

	// Go to the second page.
	deliverAll(l, l.Update(keyNextPage))

	// A load is built, then the list shrinks before it arrives.
	load := l.updateCommitsCmd()
	before := l.selector.PerPage()
	l.SetSize(80, 20)
	after := l.selector.PerPage()
	if after >= before {
		t.Fatalf("per page %d after shrinking, %d before; want fewer", after, before)
	}
	deliverAll(l, l.Update(load()))

	items := l.selector.Items()
	if len(items) != after {
		t.Fatalf("%d rows after the resize; want the %d a page holds", len(items), after)
	}
	for i, it := range items {
		if it, ok := it.(LogItem); !ok || it.Commit == nil {
			t.Fatalf("row %d is not loaded after the resize", i)
		}
	}

	// A footer toggle resizes the list after the Log handles it; the page
	// is loaded once, by the new size.
	cmd := l.Update(footer.ToggleFooterMsg{})
	l.SetSize(80, 30)
	loads := 0
	for msgs := runCmd(cmd); len(msgs) > 0; msgs = msgs[1:] {
		if _, ok := msgs[0].(LogItemsMsg); ok {
			loads++
		}
		msgs = append(msgs, runCmd(l.Update(msgs[0]))...)
	}
	if loads != 1 {
		t.Fatalf("a footer toggle loaded the page %d times; want once", loads)
	}
	if n := len(l.selector.Items()); n != l.selector.PerPage() {
		t.Fatalf("%d rows after the footer toggle; want the %d a page holds", n, l.selector.PerPage())
	}
}

// newTestLog returns a Log showing the commit list of a new two-commit
// repository at dir.
func newTestLog(t *testing.T, dir string) *Log {
	t.Helper()
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	l := NewLog(common.NewCommon(ctx, 80, 40))
	l.SetSize(80, 40)
	l.Update(RepoMsg(openRepo{path: dir}))
	l.Update(RefMsg{Ref: ref})
	l.Update(l.countCommitsCmd()())
	l.Update(l.updateCommitsCmd()())
	if l.activeView != logViewCommits {
		t.Fatalf("after loading commits, view = %v; want commits", l.activeView)
	}
	return l
}

// resizeLog resizes l the way the repository page does: it sets the size,
// then passes the window size on.
func resizeLog(l *Log, width, height int) tea.Cmd {
	l.SetSize(width, height)
	return l.Update(tea.WindowSizeMsg{Width: width, Height: height})
}

// Resizing the terminal, or toggling the footer, while a commit's diff is
// loading or shown reloads the commit list behind it but keeps the diff.
func TestLogResizeKeepsDiff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	l := newTestLog(t, dir)

	l.Update(selector.SelectMsg{IdentifiableItem: l.selector.SelectedItem()})
	// A resize while the diff loads must not show the commit list.
	resizeLog(l, 100, 30)
	l.Update(l.updateCommitsCmd()())
	if l.activeView != logViewLoading {
		t.Fatalf("after a resize while loading a diff, view = %v; want loading", l.activeView)
	}
	updateRendered(l, l.loadDiffCmd()())
	if l.activeView != logViewDiff {
		t.Fatalf("after loading the diff, view = %v; want diff", l.activeView)
	}

	for _, msg := range []tea.WindowSizeMsg{
		{Width: 120, Height: 20},
		{Width: 60, Height: 50},
	} {
		resizeLog(l, msg.Width, msg.Height)
		if l.activeView != logViewDiff {
			t.Fatalf("after %T, view = %v; want diff", msg, l.activeView)
		}
		l.Update(l.updateCommitsCmd()())
		if l.activeView != logViewDiff {
			t.Fatalf("after reloading commits on %T, view = %v; want diff", msg, l.activeView)
		}
	}

	l.Update(GoBackMsg{})
	if l.activeView != logViewCommits {
		t.Fatalf("after going back, view = %v; want commits", l.activeView)
	}
	// In the commits view a resize still shows the spinner until the
	// reloaded page arrives.
	resizeLog(l, 80, 40)
	if l.activeView != logViewLoading {
		t.Fatalf("after a resize in the commits view, view = %v; want loading", l.activeView)
	}
	l.Update(l.updateCommitsCmd()())
	if l.activeView != logViewCommits {
		t.Fatalf("after reloading commits, view = %v; want commits", l.activeView)
	}
}

// A diff that fails to load returns to the commit list and reports the error,
// so no spinner is left behind once the error is dismissed.
func TestLogDiffErrorReturnsToCommits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	l := newTestLog(t, dir)

	l.Update(selector.SelectMsg{IdentifiableItem: l.selector.SelectedItem()})
	// The repository can no longer be opened when the diff loads.
	l.Update(RepoMsg(openRepo{path: filepath.Join(dir, "missing")}))
	msg := l.loadDiffCmd()()
	if _, ok := msg.(LogDiffErrorMsg); !ok {
		t.Fatalf("loadDiffCmd() = %T; want LogDiffErrorMsg", msg)
	}
	cmd := l.Update(msg)
	if l.activeView != logViewCommits || l.selectedCommit != nil {
		t.Fatalf("after a diff error, view = %v, selected = %v; want commits, none", l.activeView, l.selectedCommit)
	}
	if cmd == nil {
		t.Fatal("after a diff error, no command; want one reporting the error")
	}
	if _, ok := cmd().(common.ErrorMsg); !ok {
		t.Fatal("after a diff error, command does not report a common.ErrorMsg")
	}
	// Repo can deliver the message again; that must not report it twice.
	if cmd := l.Update(msg); cmd != nil {
		if _, ok := cmd().(common.ErrorMsg); ok {
			t.Fatal("a repeated diff error reported the error again")
		}
	}

	// A resize after the error is dismissed shows the commit list again.
	l.Update(RepoMsg(openRepo{path: dir}))
	resizeLog(l, 100, 30)
	l.Update(l.updateCommitsCmd()())
	if l.activeView != logViewCommits {
		t.Fatalf("after a resize following a diff error, view = %v; want commits", l.activeView)
	}
}

// The mouse wheel scrolls the commit list.
func TestLogMouseWheel(t *testing.T) {
	t.Parallel()
	l := NewLog(common.NewCommon(context.Background(), 80, 24))
	l.selector.SetItems([]selector.IdentifiableItem{
		LogItem{&git.Commit{Message: "one"}},
		LogItem{&git.Commit{Message: "two"}},
	})

	l.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if got := l.selector.Index(); got != 1 {
		t.Fatalf("index after wheel down = %d; want 1", got)
	}
}
