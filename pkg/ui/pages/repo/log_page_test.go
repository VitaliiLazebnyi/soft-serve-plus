package repo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

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
	repo := newDiskRepo(t, "r", "main", "# r\n")
	for i := range 40 {
		gitRun(t, repo.path, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%02d", i))
	}
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	l := NewLog(common.NewCommon(ctx, 80, 15))
	l.SetSize(80, 15)
	l.Update(RepoMsg(repo))
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
