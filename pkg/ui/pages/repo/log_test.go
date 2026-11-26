package repo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/git/gittest"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/footer"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

// Toggling the footer resizes the commit list only after the Log pane has
// built its load, so a load paged by the old size is loaded again by the size
// the list has when it arrives, or the page shown after the resize has rows
// that were never loaded.
func TestLogReloadsPageLoadedForAnotherSize(t *testing.T) {
	t.Parallel()
	repo := newDiskRepo(t, "r", "main", "# r\n")
	for range 40 {
		gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", "c")
	}

	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	l := NewLog(c)
	l.SetSize(80, 40)
	l.Update(OpenedMsg(repo))
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
	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
	l.SetSize(80, 40)
	l.Update(OpenedMsg(openRepo{path: dir}))
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
	t.Parallel()
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
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	l := newTestLog(t, dir)

	l.Update(selector.SelectMsg{IdentifiableItem: l.selector.SelectedItem()})
	// The repository can no longer be opened when the diff loads.
	l.Update(OpenedMsg(openRepo{path: filepath.Join(dir, "missing")}))
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
	l.Update(OpenedMsg(openRepo{path: dir}))
	resizeLog(l, 100, 30)
	l.Update(l.updateCommitsCmd()())
	if l.activeView != logViewCommits {
		t.Fatalf("after a resize following a diff error, view = %v; want commits", l.activeView)
	}
}

// The mouse wheel scrolls the commit list.
func TestLogMouseWheel(t *testing.T) {
	t.Parallel()
	l := NewLog(uitest.NewCommon(t, context.Background(), nil, nil))
	l.SetSize(80, 24)
	l.selector.SetItems([]selector.IdentifiableItem{
		LogItem{&git.Commit{Message: "one"}},
		LogItem{&git.Commit{Message: "two"}},
	})

	l.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if got := l.selector.Index(); got != 1 {
		t.Fatalf("index after wheel down = %d; want 1", got)
	}
}

var (
	keyNextPage = tea.KeyPressMsg{Code: 'f', Text: "f"}
	keyPrevPage = tea.KeyPressMsg{Code: 'b', Text: "b"}
	keyDown     = tea.KeyPressMsg{Code: tea.KeyDown}
	keyUp       = tea.KeyPressMsg{Code: tea.KeyUp}
	keyHome     = tea.KeyPressMsg{Code: tea.KeyHome}
	keyEnd      = tea.KeyPressMsg{Code: tea.KeyEnd}
)

// pagedSubjects are the subjects of the commits of newPagedLog's repository,
// newest first, as the Commits tab lists them.
var pagedSubjects = func() []string {
	s := make([]string, 0, 41)
	for i := 39; i >= 0; i-- {
		s = append(s, fmt.Sprintf("c%02d", i))
	}
	return append(s, "first")
}()

// newPagedLog returns a Log, 15 rows high so that it lists 5 commits a page,
// showing the first page of a repository of 41 commits.
func newPagedLog(t *testing.T) *Log {
	t.Helper()
	return newLogOf(t, 41)
}

// newLogOf returns a Log, 15 rows high so that it lists 5 commits a page,
// showing the first page of a repository of n commits, n at least 1: "first"
// and then c00, c01 and so on.
func newLogOf(t *testing.T, n int) *Log {
	t.Helper()
	repo := newDiskRepo(t, "r", "main", "# r\n")
	for i := range n - 1 {
		gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%02d", i))
	}
	ctx := context.Background()
	l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
	l.SetSize(80, 15)
	l.Update(OpenedMsg(repo))
	ref, ok := updateRefCmd(t.Context(), repo, 1)().(RefMsg)
	if !ok {
		t.Fatal("updateRefCmd did not load the reference")
	}
	deliverAll(l, l.Update(ref))
	if l.activeView != logViewCommits {
		t.Fatalf("after loading, view = %v; want the commits", l.activeView)
	}
	if per := l.selector.PerPage(); per != 5 {
		t.Fatalf("%d commits a page; want 5", per)
	}
	return l
}

// wantPage fails unless the Log shows page (from 1) of total, with the cursor
// on the commit at index of the whole history.
func wantPage(t *testing.T, l *Log, name string, page, total, index int) {
	t.Helper()
	if got, want := l.StatusBarInfo(), fmt.Sprintf("p. %d/%d", page, total); got != want {
		t.Fatalf("%s: status %q; want %q", name, got, want)
	}
	view := ansi.Strip(l.View())
	per := l.selector.PerPage()
	for i := (page - 1) * per; i < page*per && i < len(pagedSubjects); i++ {
		if !strings.Contains(view, pagedSubjects[i]) {
			t.Fatalf("%s: page %d lacks %q:\n%s", name, page, pagedSubjects[i], view)
		}
	}
	it, ok := l.selector.SelectedItem().(LogItem)
	if !ok {
		t.Fatalf("%s: no commit under the cursor", name)
	}
	if got := strings.TrimSpace(it.Commit.Message); got != pagedSubjects[index] {
		t.Fatalf("%s: cursor on %q; want %q", name, got, pagedSubjects[index])
	}
}

// The page keys, the cursor keys at the edge of a page and the mouse wheel
// turn the page, and the status bar numbers it.
func TestLogPageKeys(t *testing.T) {
	t.Parallel()
	l := newPagedLog(t)
	press := func(msg tea.Msg) { deliverAll(l, l.Update(msg)) }

	wantPage(t, l, "start", 1, 9, 0)
	press(keyNextPage)
	wantPage(t, l, "next page", 2, 9, 5)
	press(keyDown)
	press(keyDown)
	press(keyNextPage)
	wantPage(t, l, "next page keeps the row", 3, 9, 12)
	press(keyPrevPage)
	wantPage(t, l, "previous page keeps the row", 2, 9, 7)
	press(keyUp)
	press(keyUp)
	press(keyUp)
	wantPage(t, l, "up from the first row", 1, 9, 4)
	press(keyDown)
	wantPage(t, l, "down from the last row", 2, 9, 5)
	press(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	wantPage(t, l, "wheel up from the first row", 1, 9, 4)
	press(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	wantPage(t, l, "wheel down from the last row", 2, 9, 5)
	press(keyEnd)
	wantPage(t, l, "end", 9, 9, 40)
	press(keyNextPage)
	press(keyDown)
	wantPage(t, l, "past the end", 9, 9, 40)
	press(keyHome)
	wantPage(t, l, "home", 1, 9, 0)
	press(keyPrevPage)
	press(keyUp)
	wantPage(t, l, "before the start", 1, 9, 0)
}

// A load of an earlier page that finishes after the load of the page the
// user turned to is dropped: the page turned to stays on screen.
func TestLogLatePageDropped(t *testing.T) {
	t.Parallel()
	l := newPagedLog(t)
	deliverAll(l, l.Update(keyNextPage))
	wantPage(t, l, "next page", 2, 9, 5)

	// With a commit's diff open, a resize loads the page behind it again.
	// The load finishes, but its result is queued behind what follows.
	deliverAll(l, l.Update(keyEnter))
	if l.activeView != logViewDiff {
		t.Fatalf("after enter, view = %v; want the diff", l.activeView)
	}
	l.SetSize(80, 12)
	var late []tea.Msg
	for _, msg := range runCmd(l.Update(tea.WindowSizeMsg{Width: 80, Height: 12})) {
		if _, ok := msg.(LogItemsMsg); ok {
			late = append(late, msg)
		}
	}
	if len(late) != 1 {
		t.Fatalf("a resize loaded %d pages; want one", len(late))
	}

	// Back on the list, the user turns the page.
	l.Update(GoBackMsg{})
	deliverAll(l, l.Update(keyNextPage))
	want := l.StatusBarInfo()
	view := ansi.Strip(l.View())
	l.Update(late[0])
	if got := l.StatusBarInfo(); got != want {
		t.Fatalf("after the late load, status %q; want %q", got, want)
	}
	if got := ansi.Strip(l.View()); got != view {
		t.Fatalf("the late load changed the page from\n%s\nto\n%s", view, got)
	}
	if !strings.Contains(view, "c") || l.selector.SelectedItem() == nil {
		t.Fatalf("the page turned to is blank:\n%s", view)
	}
}

// A history of a million commits is listed a page at a time: neither the
// count nor a page turn holds a slot for every commit.
//
// Not parallel: it measures the bytes the process allocates.
func TestLogMillionCommitsHoldsOnePage(t *testing.T) {
	l := newPagedLog(t)
	const limit = 4 << 20

	n := allocated(func() {
		deliverAll(l, l.Update(LogCountMsg{count: 1_000_000, gen: l.gen}))
	})
	if n > limit {
		t.Fatalf("a count of a million commits allocated %d KiB; want at most %d", n>>10, limit>>10)
	}
	wantPage(t, l, "count", 1, 200_000, 0)

	n = allocated(func() {
		deliverAll(l, l.Update(keyNextPage))
	})
	if n > limit {
		t.Fatalf("a page turn allocated %d KiB; want at most %d", n>>10, limit>>10)
	}
	wantPage(t, l, "page turn", 2, 200_000, 5)
}

// A resize that keeps the number of commits a page holds, such as a change
// of width, loads nothing and keeps the page on screen.
func TestLogResizeKeepingPageSizeLoadsNothing(t *testing.T) {
	t.Parallel()
	l := newPagedLog(t)
	deliverAll(l, l.Update(keyNextPage))
	for _, msg := range runCmd(resizeLog(l, 100, 16)) {
		if _, ok := msg.(LogItemsMsg); ok {
			t.Fatal("a resize that keeps the page size loaded the page")
		}
	}
	wantPage(t, l, "after the resize", 2, 9, 5)
	if l.activeView != logViewCommits {
		t.Fatalf("after the resize, view = %v; want the commits", l.activeView)
	}
}

// End, then a resize before the last page arrives, shows the last page by the
// new size with the cursor on the last commit, not a page past the end.
func TestLogEndThenResize(t *testing.T) {
	t.Parallel()
	l := newPagedLog(t)
	end := l.Update(keyEnd)
	deliverAll(l, resizeLog(l, 80, 9))
	deliverAll(l, end)
	if per := l.selector.PerPage(); per != 3 {
		t.Fatalf("%d commits a page after the resize; want 3", per)
	}
	wantPage(t, l, "end, then a resize", 14, 14, 40)
}

// Next page keeps the row, which can be past the last commit of a short last
// page. A resize before that page arrives puts the cursor on the last commit,
// not on a page past the end.
func TestLogNextPageThenResize(t *testing.T) {
	t.Parallel()
	l := newPagedLog(t)
	for range 7 {
		deliverAll(l, l.Update(keyNextPage))
	}
	for range 4 {
		deliverAll(l, l.Update(keyDown))
	}
	wantPage(t, l, "row 4 of page 8", 8, 9, 39)
	next := l.Update(keyNextPage)
	deliverAll(l, resizeLog(l, 80, 9))
	deliverAll(l, next)
	if per := l.selector.PerPage(); per != 3 {
		t.Fatalf("%d commits a page after the resize; want 3", per)
	}
	wantPage(t, l, "next, then a resize", 14, 14, 40)
}

// At the edges of a short history the status bar numbers the one or two
// pages right, and the keys that would leave them do nothing.
func TestLogPageEdges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		commits, rows   int
		first, lastPage string
	}{
		{0, 0, "p. 1/1", "p. 1/1"},
		{1, 1, "p. 1/1", "p. 1/1"},
		{5, 5, "p. 1/1", "p. 1/1"},
		{6, 5, "p. 1/2", "p. 2/2"},
	} {
		t.Run(fmt.Sprint(tc.commits), func(t *testing.T) {
			t.Parallel()
			l := newLogOf(t, max(tc.commits, 1))
			if tc.commits == 0 {
				deliverAll(l, l.Update(LogCountMsg{count: 0, gen: l.gen}))
			}
			if got := l.StatusBarInfo(); got != tc.first {
				t.Fatalf("status %q; want %q", got, tc.first)
			}
			if n := len(l.selector.Items()); n != tc.rows {
				t.Fatalf("%d rows; want %d", n, tc.rows)
			}
			pages := tc.commits > l.selector.PerPage()
			if got := helpHasDesc(l.FullHelp(), "next page"); got != pages {
				t.Fatalf("help lists the page keys: %v; want %v", got, pages)
			}
			deliverAll(l, l.Update(keyEnd))
			want := l.selector.SelectedItem()
			for _, k := range []tea.Msg{keyNextPage, keyDown, keyEnd, tea.MouseWheelMsg{Button: tea.MouseWheelDown}} {
				for _, msg := range runCmd(l.Update(k)) {
					if _, ok := msg.(LogItemsMsg); ok {
						t.Fatalf("%v on the last page loaded a page", k)
					}
				}
				if got := l.StatusBarInfo(); got != tc.lastPage {
					t.Fatalf("after %v on the last page, status %q; want %q", k, got, tc.lastPage)
				}
				if got := l.selector.SelectedItem(); got != want {
					t.Fatalf("after %v on the last page, the cursor moved", k)
				}
			}
			if tc.commits > 0 {
				if got := strings.TrimSpace(want.(LogItem).Commit.Message); got != "first" {
					t.Fatalf("the cursor is on %q at the end; want the oldest commit", got)
				}
			}
		})
	}
}

// Toggling the footer while another tab is shown still loads the Commits
// tab's page again by its new size, so the tab does not show a page of the
// wrong length when it is shown next.
func TestRepoFooterToggleReachesHiddenCommitsTab(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	for i := range 20 {
		gitRun(t, p.repo.path, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%02d", i))
	}
	p.open()
	p.tab(1)
	before := p.log.selector.PerPage()
	if n := len(p.log.selector.Items()); n != before {
		t.Fatalf("%d commits listed; want a full page of %d", n, before)
	}

	// The footer takes rows from the page, which its parent resizes before
	// it passes the toggle on.
	p.r.SetSize(120, 30)
	p.settle(footer.ToggleFooterMsg{})
	after := p.log.selector.PerPage()
	if after >= before {
		t.Fatalf("%d commits a page after the footer shows, %d before; want fewer", after, before)
	}
	if n := len(p.log.selector.Items()); n != after {
		t.Fatalf("the hidden Commits tab lists %d commits after the footer toggle; want the %d a page holds", n, after)
	}
}

// The Log tab of a repository whose history holds a commit git-module
// cannot parse lists the other commits instead of ending the session with a
// panic.
func TestLogSkipsMalformedCommit(t *testing.T) {
	t.Parallel()
	for _, kind := range gittest.Malformed {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "r.git")
			gittest.Git(t, "", nil, "init", "-q", "--bare", dir)
			root := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "root\n"), "root")
			bad := gittest.Commit(t, dir, kind, gittest.Tree(t, dir, "f", "bad\n"), "bad", root)
			top := gittest.Commit(t, dir, gittest.Valid, gittest.Tree(t, dir, "f", "top\n"), "top", bad)
			gittest.Git(t, dir, nil, "update-ref", "refs/heads/main", top)
			gittest.Git(t, dir, nil, "symbolic-ref", "HEAD", "refs/heads/main")

			r, err := git.Open(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := r.HEAD()
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
			l.SetSize(80, 40)
			l.Update(OpenedMsg(openRepo{path: dir}))
			l.Update(RefMsg{Ref: ref})
			l.Update(l.countCommitsCmd()())
			msg := l.updateCommitsCmd()()
			items, ok := msg.(LogItemsMsg)
			if !ok {
				t.Fatalf("loading commits = %T %v; want LogItemsMsg", msg, msg)
			}
			l.Update(items)
			l.View()

			var got []string
			for _, it := range items.items {
				if li, ok := it.(LogItem); ok && li.Commit != nil {
					got = append(got, li.Commit.ID.String())
				}
			}
			if len(got) != 2 || got[0] != top || got[1] != root {
				t.Fatalf("listed %v, want the valid commits %v", got, []string{top, root})
			}
		})
	}
}

// renameRepo makes a repository at dir whose HEAD renames from to to, with
// names staged straight into the index so any name works.
func renameRepo(t *testing.T, dir, from, to string) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "core.protectNTFS=false"}, args...)...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("", "init", "-q", "-b", "main")
	body := strings.Repeat("same line\n", 20)
	put := func(name, content string) {
		blob := run(content, "hash-object", "-w", "--stdin")
		run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	put(from, body)
	run("", "commit", "-q", "-m", "add")
	run("", "update-index", "--force-remove", "--", from)
	put(to, body+"changed\n")
	run("", "commit", "-q", "-m", "rename")
}

// Opening a commit that renames a file to or from a name git quotes
// crashed the server from the diff parser's goroutine. The Log page must
// show the diff with the rename.
func TestLogDiffRenameQuotedName(t *testing.T) {
	t.Parallel()
	names := []string{"café.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\there.txt", `quo"te.txt`, `back\slash.txt`)
	}
	for _, name := range names {
		for _, d := range []struct{ from, to string }{{"plain.txt", name}, {name, "plain.txt"}} {
			t.Run(fmt.Sprintf("%q to %q", d.from, d.to), func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(t.TempDir(), "repo")
				renameRepo(t, dir, d.from, d.to)
				r, err := git.Open(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := r.HEAD()
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
				l.SetSize(120, 60)
				l.Update(OpenedMsg(openRepo{path: dir}))
				l.Update(RefMsg{Ref: ref})
				l.Update(l.countCommitsCmd()())
				l.Update(l.updateCommitsCmd()())

				// The newest commit, the rename, is selected first.
				l.Update(selector.SelectMsg{IdentifiableItem: l.selector.SelectedItem()})
				msg := l.loadDiffCmd()()
				if _, ok := msg.(LogDiffMsg); !ok {
					t.Fatalf("loadDiffCmd() = %#v; want LogDiffMsg", msg)
				}
				updateRendered(l, msg)
				if l.activeView != logViewDiff {
					t.Fatalf("view = %v; want the diff", l.activeView)
				}
				if !strings.Contains(l.currentDiff.Patch, "rename from ") || !strings.Contains(l.currentDiff.Patch, "+changed") {
					t.Errorf("diff does not show the rename:\n%s", l.currentDiff.Patch)
				}
			})
		}
	}
}

// TestWrapTextMeasuresLikeLipgloss checks that commit text wraps at the width
// lipgloss measures, so wide characters such as emoji never overflow the pane.
func TestWrapTextMeasuresLikeLipgloss(t *testing.T) {
	t.Parallel()
	const width = 9
	in := "fix: 🎉🎉🎉 ship it 🚀🚀🚀🚀 now\nsecond\tline 👍🏽👍🏽👍🏽👍🏽"
	out := wrapText(in, width)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("line %q is %d columns wide, want at most %d", line, w, width)
		}
	}
	if strings.Contains(out, "\t") {
		t.Errorf("tab not expanded: %q", out)
	}
	if got, want := strings.ReplaceAll(strings.ReplaceAll(out, "\n", ""), " ", ""),
		strings.NewReplacer("\n", "", " ", "", "\t", "").Replace(in); got != want {
		t.Errorf("wrapping lost text: got %q, want %q", got, want)
	}
	if got := wrapText("unchanged", 0); got != "unchanged" {
		t.Errorf("width 0: got %q, want input unchanged", got)
	}
}
