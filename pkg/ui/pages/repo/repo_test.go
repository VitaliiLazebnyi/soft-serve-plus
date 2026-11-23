package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

type fakeRepo struct {
	private     bool
	projectName string
	description string
}

func (r fakeRepo) ID() int64                                     { return 1 }
func (r fakeRepo) Name() string                                  { return "repo" }
func (r fakeRepo) ProjectName() string                           { return r.projectName }
func (r fakeRepo) Description() string                           { return r.description }
func (r fakeRepo) IsPrivate() bool                               { return r.private }
func (r fakeRepo) IsMirror() bool                                { return false }
func (r fakeRepo) IsHidden() bool                                { return false }
func (r fakeRepo) UserID() int64                                 { return 1 }
func (r fakeRepo) CreatedAt() time.Time                          { return time.Time{} }
func (r fakeRepo) UpdatedAt() time.Time                          { return time.Time{} }
func (r fakeRepo) Open(context.Context) (*git.Repository, error) { return nil, nil }

var _ proto.Repository = fakeRepo{}

// The repository page header shows the repository's visibility next to its
// name, public as well as private.
func TestHeaderVisibilityMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		private      bool
		want, absent string
	}{
		{private: true, want: "🔒", absent: "🌐"},
		{private: false, want: "🌐", absent: "🔒"},
	} {
		r := &Repo{
			common:       uitest.NewCommon(t, context.Background(), nil, nil),
			width:        80,
			height:       24,
			selectedRepo: fakeRepo{private: tc.private},
		}
		header := r.headerView()
		if !strings.Contains(header, tc.want) {
			t.Errorf("private=%v: header %q lacks %q", tc.private, header, tc.want)
		}
		if strings.Contains(header, tc.absent) {
			t.Errorf("private=%v: header %q shows %q", tc.private, header, tc.absent)
		}
	}
}

// Switching to a tab with nothing to show, such as an empty Tags tab, must
// not keep the previous tab's status value, and a copy notice must go away
// once the status is refreshed.
func TestStatusBarDoesNotKeepStaleValues(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	r := New(c, NewLog(c), NewRefs(c, git.RefsTags))
	r.SetSize(120, 24)
	r.selectedRepo = fakeRepo{}
	r.state = readyState

	r.statusbar.SetStatus("repo", "abc1234 by Alice", "p. 2/3", "*")
	r.activeTab = 1
	r.setStatusBarInfo()
	view := r.statusbar.View()
	for _, stale := range []string{"abc1234", "p. 2/3"} {
		if strings.Contains(view, stale) {
			t.Errorf("status bar still shows %q:\n%s", stale, view)
		}
	}

	r.Update(CopyMsg{Text: "x", Message: "Reference copied to clipboard"})
	if view := r.statusbar.View(); !strings.Contains(view, "Reference copied to clipboard") {
		t.Errorf("status bar lacks the copy notice:\n%s", view)
	}
	r.setStatusBarInfo()
	if view := r.statusbar.View(); strings.Contains(view, "copied") {
		t.Errorf("copy notice stuck in the status bar:\n%s", view)
	}
}

// These tests drive the repository page the way the bubbletea runtime does:
// they send messages, run the commands Update returns, and look at View. They
// pin what the user sees on each tab, so the components can be reworked
// underneath.

var (
	keyTab      = tea.KeyPressMsg{Code: tea.KeyTab}
	keyShiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	keyEnter    = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc      = tea.KeyPressMsg{Code: tea.KeyEscape}
	keyB        = tea.KeyPressMsg{Code: 'b', Text: "b"}
)

// pinPage is a repository page over a repository on disk with every tab soft
// browse shows.
type pinPage struct {
	t        *testing.T
	r        *Repo
	repo     diskRepo
	readme   *Readme
	files    *Files
	log      *Log
	stash    *Stash
	branches *Refs
	tags     *Refs
}

// newPinRepo creates a repository with a README, a file changed in a second
// commit, a directory, a branch, a tag and a stash.
func newPinRepo(t *testing.T) diskRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pin")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# Pin\n\nreadme-body\n")
	write("file.txt", "file-body-one\n")
	write("sub/inner.txt", "inner-body\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "first-subject")
	write("file.txt", "file-body-two\n")
	gitRun(t, dir, "commit", "-q", "-am", "second-subject")
	gitRun(t, dir, "branch", "dev")
	gitRun(t, dir, "tag", "v1")
	write("file.txt", "stashed-body\n")
	gitRun(t, dir, "stash", "push", "-q", "-m", "stash-subject")
	return diskRepo{name: "pin", path: dir}
}

func newPinPage(t *testing.T) *pinPage {
	t.Helper()
	// The Commits tab shows its spinner once a load is older than
	// waitBeforeLoading, judged by the clock. A negative wait makes every
	// load old enough on any clock: with a zero wait a load is not yet
	// older than zero when the clock returns the same instant again, as
	// Windows' does within its resolution.
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	p := &pinPage{
		t:        t,
		repo:     newPinRepo(t),
		readme:   NewReadme(c),
		files:    NewFiles(c),
		log:      NewLog(c),
		stash:    NewStash(c),
		branches: NewRefs(c, git.RefsHeads),
		tags:     NewRefs(c, git.RefsTags),
	}
	p.log.waitBeforeLoading = -time.Hour
	p.r = New(c, p.readme, p.files, p.log, p.stash, p.branches, p.tags)
	p.r.SetSize(120, 40)
	return p
}

// update delivers msg to the page and returns the messages its command
// produces, without delivering them.
func (p *pinPage) update(msg tea.Msg) []tea.Msg {
	p.t.Helper()
	cmd := p.r.Update(msg)
	return runCmd(cmd)
}

// settle delivers msgs and then every message their commands produce, until
// none is left. Spinner ticks are dropped: spinners tick for as long as
// something loads.
func (p *pinPage) settle(msgs ...tea.Msg) {
	p.t.Helper()
	// The last tick dropped is delivered once everything else is, as the
	// runtime would, so that the spinner sees nothing loads any more.
	var tick tea.Msg
	defer func() {
		if tick != nil {
			p.r.Update(tick)
		}
	}()
	for steps := 0; len(msgs) > 0; steps++ {
		if steps > 10000 {
			p.t.Fatal("page never settles")
		}
		var msg tea.Msg
		msg, msgs = msgs[0], msgs[1:]
		switch msg := msg.(type) {
		case spinner.TickMsg:
			tick = msg
			continue
		case common.ErrorMsg:
			p.t.Fatalf("page reported an error: %v", msg.Err)
		}
		msgs = append(msgs, p.update(msg)...)
	}
}

// open shows the repository and waits for every tab to load.
func (p *pinPage) open() {
	p.t.Helper()
	p.settle(OpenedMsg(p.repo))
}

func (p *pinPage) view() string {
	return ansi.Strip(p.r.View())
}

// wantView fails unless the view holds every one of want.
func (p *pinPage) wantView(name string, want ...string) {
	p.t.Helper()
	v := p.view()
	for _, w := range want {
		if !strings.Contains(v, w) {
			p.t.Fatalf("%s: view lacks %q:\n%s", name, w, v)
		}
	}
}

// notView fails if the view holds any one of bad.
func (p *pinPage) notView(name string, bad ...string) {
	p.t.Helper()
	v := p.view()
	for _, b := range bad {
		if strings.Contains(v, b) {
			p.t.Fatalf("%s: view has %q:\n%s", name, b, v)
		}
	}
}

// tab makes tab i the active one with the mouse-free path a user takes:
// repeated tab presses.
func (p *pinPage) tab(i int) {
	p.t.Helper()
	for p.r.activeTab != i {
		p.settle(keyTab)
	}
}

// selectItem highlights the item of s whose ID is id.
func selectItem(t *testing.T, s *selector.Selector, id string) {
	t.Helper()
	for i, it := range s.Items() {
		if it.(selector.IdentifiableItem).ID() == id {
			s.Select(i)
			return
		}
	}
	t.Fatalf("no item %q", id)
}

func selectTab(i int) tea.Msg { return tabs.SelectTabMsg(i) }

func ticks(msgs []tea.Msg) []tea.Msg {
	var out []tea.Msg
	for _, m := range msgs {
		if _, ok := m.(spinner.TickMsg); ok {
			out = append(out, m)
		}
	}
	return out
}

func nonTicks(msgs []tea.Msg) []tea.Msg {
	var out []tea.Msg
	for _, m := range msgs {
		if _, ok := m.(spinner.TickMsg); !ok {
			out = append(out, m)
		}
	}
	return out
}

// Tab and shift+tab walk the tabs, and each tab shows its content.
func TestPinTabSwitching(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()

	want := []string{"readme-body", "file.txt", "second-subject", "stash-subject", "dev", "v1"}
	for i, w := range want {
		if p.r.activeTab != i {
			t.Fatalf("active tab = %d; want %d", p.r.activeTab, i)
		}
		p.wantView("tab "+w, w)
		p.notView("tab "+w, "loading")
		p.settle(keyTab)
	}
	if p.r.activeTab != 0 {
		t.Fatalf("tab after the last = %d; want 0", p.r.activeTab)
	}
	p.settle(keyShiftTab)
	if p.r.activeTab != len(want)-1 {
		t.Fatalf("shift+tab from the first tab = %d; want %d", p.r.activeTab, len(want)-1)
	}
	p.wantView("shift+tab", "v1")
	p.settle(keyShiftTab)
	p.wantView("shift+tab twice", "dev", "main")
}

// Opening and leaving items on each tab.
func TestPinSelection(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()

	// Files: a file, its blame, and back; a directory and back.
	p.tab(1)
	p.wantView("file list", "README.md", "file.txt", "sub")
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	p.wantView("file", "file-body-two")
	if p.r.Path() == "" {
		t.Fatal("no path with a file open")
	}
	msgs := p.update(keyB)
	p.wantView("blame loading", "loading")
	p.settle(msgs...)
	p.wantView("blame", "second-subject", "file-body-two")
	p.notView("blame", "loading")
	p.settle(keyB)
	p.notView("blame off", "second-subject", "loading")
	p.settle(keyEsc)
	p.wantView("back to the file list", "README.md", "file.txt", "sub")
	if p.r.Path() != "" {
		t.Fatalf("path %q after going back", p.r.Path())
	}
	selectItem(t, p.files.selector, "sub")
	p.settle(keyEnter)
	p.wantView("directory", "inner.txt")
	p.notView("directory", "README.md")
	p.settle(keyEsc)
	p.wantView("back from the directory", "README.md", "sub")

	// Commits: a commit's diff and back.
	p.tab(2)
	msgs = p.update(keyEnter)
	p.settle(nonTicks(msgs)...)
	p.wantView("commit", "commit ", "second-subject", "file-body-two")
	p.settle(keyEsc)
	p.wantView("back to the commits", "first-subject", "second-subject")
	p.notView("back to the commits", "file-body-two")

	// Stash: a stash's patch and back.
	p.tab(3)
	p.settle(keyEnter)
	p.wantView("stash patch", "stash-subject", "stashed-body")
	p.settle(keyEsc)
	p.wantView("back to the stashes", "stash-subject")
	p.notView("back to the stashes", "stashed-body")

	// Branches: picking a branch shows its files.
	p.tab(4)
	selectItem(t, p.branches.selector, git.RefsHeads+"dev")
	p.settle(keyEnter)
	if p.r.activeTab != 1 {
		t.Fatalf("after picking a branch, tab = %d; want files", p.r.activeTab)
	}
	if p.r.ref == nil || p.r.ref.Name().Short() != "dev" {
		t.Fatalf("ref after picking dev = %v", p.r.ref)
	}
	p.wantView("files of dev", "file.txt", "dev")
}

// Each tab shows a spinner while it loads; the spinner turns while it does
// and stops once everything has loaded.
func TestPinLoadingAndSpinner(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.r.Init()

	// The page loads the reference first.
	msgs := p.update(OpenedMsg(p.repo))
	p.wantView("repository", "loading")
	pending := ticks(msgs)
	if len(pending) == 0 {
		t.Fatal("no spinner started while the reference loads")
	}
	before := p.view()
	var next []tea.Msg
	for _, tk := range pending {
		next = append(next, ticks(p.update(tk))...)
	}
	if p.view() == before {
		t.Fatalf("spinner did not turn:\n%s", before)
	}
	if len(next) == 0 {
		t.Fatal("spinner stopped while the reference loads")
	}

	// The reference arrives: every tab starts loading.
	var ref tea.Msg
	for _, m := range nonTicks(msgs) {
		if _, ok := m.(RefMsg); ok {
			ref = m
		}
	}
	if ref == nil {
		t.Fatalf("no RefMsg in %#v", msgs)
	}
	msgs = p.update(ref)
	results := nonTicks(msgs)
	pending = append(next, ticks(msgs)...)

	loading := []string{"loading", "loading", "loading commits", "loading", "loading", "loading"}
	for i, w := range loading {
		p.r.Update(selectTab(i))
		p.wantView("loading tab "+w, w)
		before := p.view()
		var next []tea.Msg
		for _, tk := range pending {
			next = append(next, ticks(p.update(tk))...)
		}
		if after := p.view(); after == before {
			t.Fatalf("tab %d: spinner did not turn:\n%s", i, after)
		}
		if len(next) == 0 {
			t.Fatalf("tab %d: spinner stopped while loading", i)
		}
		pending = next
	}

	// Everything loads.
	p.settle(results...)
	for i, w := range []string{"readme-body", "file.txt", "second-subject", "stash-subject", "main", "v1"} {
		p.r.Update(selectTab(i))
		p.wantView("loaded tab", w)
		p.notView("loaded tab", "loading")
	}
	for _, tk := range pending {
		if next := ticks(p.update(tk)); len(next) != 0 {
			t.Fatalf("spinner still turns with nothing loading: %#v", next)
		}
	}

	// A blame loads in the Files tab alone; the spinner turns again.
	p.r.Update(selectTab(1))
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	msgs = p.update(keyB)
	p.wantView("blame", "loading")
	pending = ticks(msgs)
	if len(pending) == 0 {
		t.Fatal("no spinner started for the blame")
	}
	before = p.view()
	for _, tk := range pending {
		p.update(tk)
	}
	if p.view() == before {
		t.Fatalf("blame spinner did not turn:\n%s", before)
	}
	p.settle(nonTicks(msgs)...)
	p.notView("blame loaded", "loading")

	// Opening a commit shows its own loading text.
	p.r.Update(selectTab(2))
	msgs = p.update(keyEnter)
	for _, m := range nonTicks(msgs) {
		if _, ok := m.(selector.SelectMsg); ok {
			p.update(m)
			break
		}
	}
	p.wantView("commit loading", "loading commit…")
}

// runCmd runs cmd and every command it batches, and returns the messages
// they produce.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, runCmd(c)...)
		}
		return msgs
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// findMsg returns the first message of type T in msgs.
func findMsg[T any](t *testing.T, msgs []tea.Msg) T {
	t.Helper()
	for _, msg := range msgs {
		if m, ok := msg.(T); ok {
			return m
		}
	}
	var zero T
	t.Fatalf("no %T in %#v", zero, msgs)
	return zero
}

// cmdTestRepo creates a repository with one commit holding a file and
// returns it with its HEAD reference.
func cmdTestRepo(t *testing.T) (openRepo, *git.Reference) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "file.txt")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	return openRepo{path: dir}, ref
}

func cmdTestCommon(t *testing.T) *common.Common {
	t.Helper()
	return uitest.NewCommon(t, context.Background(), nil, nil)
}

// bubbletea runs every command in its own goroutine, after Update returned.
// A command must therefore not read the model, which Update may have changed
// in the meantime: an empty repository or a new ref clears the file and ref
// the blame command needs, and reading them crashed the server.
func TestFilesCommandsDoNotReadModel(t *testing.T) {
	t.Parallel()
	repo, ref := cmdTestRepo(t)
	f := NewFiles(cmdTestCommon(t))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(repo))
	cmd := f.Update(RefMsg{Ref: ref})
	items := findMsg[FileItemsMsg](t, runCmd(cmd))
	f.Update(items)
	if len(items.items) != 1 {
		t.Fatalf("got %d files, want 1", len(items.items))
	}

	// Select the file, then clear the model before its command runs. The
	// load is cancelled, and its command neither crashes nor sends a
	// result.
	cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	content := FileContentMsg{content: "hello\n", path: "file.txt", load: f.load, gen: f.gen}
	f.Update(EmptyRepoMsg{})
	noLoadResult(t, runCmd(cmd))
	// The content of a file opened before the repository was emptied is
	// dropped.
	f.Update(content)
	if f.activeView == filesViewContent || f.Path() != "" {
		t.Fatalf("stale content opened %q", f.Path())
	}

	// Content requested before a ref change is dropped too.
	f.Update(RefMsg{Ref: ref})
	cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	content = FileContentMsg{content: "hello\n", path: "file.txt", load: f.load, gen: f.gen}
	f.Update(RefMsg{Ref: ref})
	noLoadResult(t, runCmd(cmd))
	f.Update(content)
	if f.activeView == filesViewContent || f.Path() != "" {
		t.Fatalf("stale content opened %q", f.Path())
	}

	// Open the file and toggle the blame view, then clear the model before
	// the blame command runs.
	cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	f.Update(findMsg[FileContentMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "file.txt" {
		t.Fatalf("path = %q, want file.txt", got)
	}
	cmd = f.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	blame := FileBlameMsg{blame: &git.Blame{}, load: f.load, gen: f.gen}
	f.Update(EmptyRepoMsg{})
	noLoadResult(t, runCmd(cmd))
	// The blame of a file opened before the repository was emptied is
	// dropped.
	f.Update(blame)
	if f.activeView == filesViewContent || f.currentBlame != nil {
		t.Fatal("a stale blame opened the content view")
	}
}

func TestLogCommandsDoNotReadModel(t *testing.T) {
	t.Parallel()
	repo, ref := cmdTestRepo(t)
	l := NewLog(cmdTestCommon(t))
	l.SetSize(80, 40)
	l.Update(OpenedMsg(repo))

	// A new ref, then the model changes before the count command runs. An
	// empty repository would cancel the load, so clear the model by hand.
	cmd := l.Update(RefMsg{Ref: ref})
	l.repo, l.ref = nil, nil
	count := findMsg[LogCountMsg](t, runCmd(cmd))
	if count.count != 1 {
		t.Fatalf("count = %d, want 1", count.count)
	}

	l.Update(OpenedMsg(repo))
	l.Update(RefMsg{Ref: ref})
	cmd = l.Update(count)
	l.repo, l.ref = nil, nil
	commits := findMsg[LogItemsMsg](t, runCmd(cmd))
	if len(commits.items) != 1 || commits.items[0] == nil {
		t.Fatalf("commits = %#v, want one commit", commits)
	}

	// Open a commit, then switch the ref before the diff command runs.
	commit := commits.items[0].(LogItem).Commit
	l.Update(OpenedMsg(repo))
	l.Update(RefMsg{Ref: ref})
	cmd = l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: commit}})
	l.selectedCommit = nil
	diff := findMsg[LogDiffMsg](t, runCmd(cmd))

	// The diff of a commit that is no longer selected is dropped.
	updateRendered(l, diff)
	if l.activeView == logViewDiff {
		t.Fatal("a stale diff opened the diff view")
	}

	// Open another commit before the diff of the first one arrives: only the
	// diff of the selected commit is shown.
	// Loading the commits again gives another *git.Commit.
	cmd = l.Update(count)
	other := findMsg[LogItemsMsg](t, runCmd(cmd)).items[0].(LogItem).Commit
	if other == commit {
		t.Fatal("reloading the commits returned the same commit")
	}
	cmd = l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: commit}})
	otherCmd := l.Update(selector.SelectMsg{IdentifiableItem: LogItem{Commit: other}})
	// The first load was cancelled by the second; had it finished, its
	// diff would still be dropped.
	noLoadResult(t, runCmd(cmd))
	updateRendered(l, LogDiffMsg{commit: commit, diff: &git.Diff{}, gen: l.gen})
	if l.activeView == logViewDiff {
		t.Fatal("the diff of a commit that is no longer selected was shown")
	}
	updateRendered(l, findMsg[LogDiffMsg](t, runCmd(otherCmd)))
	if l.activeView != logViewDiff || l.currentDiff == nil {
		t.Fatal("the diff of the selected commit was not shown")
	}
}

// Repo hands a FileContentMsg to the Files pane twice, once as the pane the
// message belongs to and once as the active tab. Opening a file must still
// push its position only once, so that going back returns the cursor to the
// directory the user opened.
func TestFilesBackAfterOpeningFileThroughRepo(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	for _, name := range []string{"a/f", "b/f", "sub/x", "sub/y", "sub/z"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	gr, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	f := NewFiles(cmdTestCommon(t))
	f.SetSize(80, 40)
	r := New(cmdTestCommon(t), f)
	r.SetSize(80, 40)
	update := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		cmd := r.Update(msg)
		return cmd
	}
	// Give the pane its repository and ref directly: Repo itself only needs
	// them for the status bar.
	f.Update(OpenedMsg(openRepo{path: dir}))
	cmd := f.Update(RefMsg{Ref: ref})
	update(findMsg[FileItemsMsg](t, runCmd(cmd)))

	// Open "sub", the third directory of the root.
	items := f.selector.Items()
	if len(items) != 3 {
		t.Fatalf("got %d root entries, want 3", len(items))
	}
	f.selector.Select(2)
	cmd = update(selector.SelectMsg{IdentifiableItem: items[2].(selector.IdentifiableItem)})
	update(findMsg[FileItemsMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "sub" {
		t.Fatalf("path = %q, want sub", got)
	}

	// Open "y", the second file of "sub", through Repo.
	items = f.selector.Items()
	f.selector.Select(1)
	cmd = update(selector.SelectMsg{IdentifiableItem: items[1].(selector.IdentifiableItem)})
	update(findMsg[FileContentMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "sub/y" {
		t.Fatalf("path = %q, want sub/y", got)
	}

	// Going back returns to "y" in "sub", then to "sub" in the root.
	for _, want := range []struct {
		path  string
		index int
	}{{"sub", 1}, {"", 2}} {
		cmd = update(GoBackMsg{})
		update(findMsg[FileItemsMsg](t, runCmd(cmd)))
		if got := f.Path(); got != want.path {
			t.Fatalf("path = %q, want %q", got, want.path)
		}
		if got := f.selector.Index(); got != want.index {
			t.Fatalf("in %q the cursor is on entry %d, want %d", want.path, got, want.index)
		}
	}
}

// noLoadResult fails if msgs holds the result of a load: a load that was
// cancelled before it ran sends nothing.
func noLoadResult(t *testing.T, msgs []tea.Msg) {
	t.Helper()
	for _, m := range msgs {
		if _, ok := m.(paneMsg); ok {
			t.Fatalf("a cancelled load sent %T", m)
		}
		if _, ok := m.(common.ErrorMsg); ok {
			t.Fatalf("a cancelled load sent an error: %v", m)
		}
	}
}

// An error from a load started for a repository or ref the user has left is
// dropped like any late result: it neither reaches a pane nor the error
// screen.
func TestRepoDropsStaleError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := newPinPage(t)
	p.open()
	fail := func(to paneID) {
		t.Helper()
		p.update(errorMsg{err: errors.New("listing failed"), to: to, gen: p.r.gen})
	}

	// Log: a page reload fails.
	p.log.SetSize(100, 30)
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

// diskRepo is a repository on disk with a name.
type diskRepo struct {
	fakeRepo
	name, path string
}

func (r diskRepo) Name() string                                      { return r.name }
func (r diskRepo) Open(ctx context.Context) (*git.Repository, error) { return git.Open(ctx, r.path) }

// newDiskRepo creates a repository with a README on its branch.
func newDiskRepo(t *testing.T, name, branch, readme string) diskRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", branch, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README.md")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	return diskRepo{name: name, path: dir}
}

// What was loaded for a repository or ref the user has left since arrives
// late and must not be shown in place of the current one.
func TestRepoDropsLateMessages(t *testing.T) {
	t.Parallel()
	a := newDiskRepo(t, "a", "main", "# Readme of A\n")
	b := newDiskRepo(t, "b", "trunk", "# Readme of B\n")
	gitRun(t, b.path, "checkout", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(b.path, "README.md"), []byte("# Readme of B dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, b.path, "commit", "-q", "-am", "dev")
	gitRun(t, b.path, "checkout", "-q", "trunk")

	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	readme := NewReadme(c)
	readme.SetSize(80, 40)
	files := NewFiles(c)
	files.SetSize(80, 40)
	r := New(c, readme, files, NewRefs(c, git.RefsHeads))
	r.SetSize(80, 40)
	// The readme tab is the active one; the page shows its spinner in
	// place of the tab while it loads.
	readmeView := func() string { return ansi.Strip(r.View()) }

	// Open A; its ref and loads start, but the user leaves before they
	// arrive.
	r.Update(OpenedMsg(a))
	refA := updateRefCmd(t.Context(), a, r.gen)()
	r.Update(refA)
	lateReadmeA := readme.updateReadmeCmd()
	lateFilesA := files.updateFilesCmd()

	// Open B.
	r.Update(OpenedMsg(b))
	refB := updateRefCmd(t.Context(), b, r.gen)()

	r.Update(refA)
	if r.ref != nil {
		t.Fatalf("late ref of A shown in B: %v", r.ref.Name())
	}
	updateRendered(r, lateReadmeA())
	if strings.Contains(readmeView(), "Readme of A") {
		t.Fatalf("late readme of A shown in B:\n%s", readmeView())
	}

	r.Update(refB)
	if r.ref == nil || r.ref.Name().Short() != "trunk" {
		t.Fatalf("ref of B = %v; want trunk", r.ref)
	}
	if files.ref == nil || files.ref.Name().Short() != "trunk" {
		t.Fatalf("files ref of B = %v; want trunk", files.ref)
	}
	updateRendered(r, lateReadmeA())
	r.Update(lateFilesA())
	if strings.Contains(readmeView(), "Readme of A") {
		t.Fatalf("late readme of A shown in B:\n%s", readmeView())
	}
	if files.activeView != filesViewLoading {
		t.Fatalf("late files of A shown in B: view %v", files.activeView)
	}

	updateRendered(r, readme.updateReadmeCmd()())
	if v := readmeView(); !strings.Contains(v, "Readme of B") {
		t.Fatalf("readme of B not shown:\n%s", v)
	}

	// Switch B to another ref; the readme of the previous one arrives
	// late.
	lateReadmeTrunk := readme.updateReadmeCmd()
	rr, err := b.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	refs, err := rr.References()
	if err != nil {
		t.Fatal(err)
	}
	var dev *git.Reference
	for _, ref := range refs {
		if ref.Name().Short() == "dev" {
			dev = ref
		}
	}
	if dev == nil {
		t.Fatal("no dev branch")
	}
	r.Update(switchRefCmd(dev, r.gen)())
	updateRendered(r, lateReadmeTrunk())
	if v := readmeView(); strings.Contains(v, "Readme of B") {
		t.Fatalf("late readme of trunk shown for dev:\n%s", v)
	}
	updateRendered(r, readme.updateReadmeCmd()())
	if v := readmeView(); !strings.Contains(v, "Readme of B dev") {
		t.Fatalf("readme of dev not shown:\n%s", v)
	}
}

func newTestCommon(t *testing.T) *common.Common {
	t.Helper()
	return uitest.NewCommon(t, context.Background(), nil, nil)
}

func helpKeys(bindings []key.Binding) []string {
	keys := make([]string, 0, len(bindings))
	for _, b := range bindings {
		h := b.Help()
		keys = append(keys, h.Key+" "+h.Desc)
	}
	return keys
}

// Turning the blame view off with "b" must not also page the file up.
func TestFilesBlameKeyDoesNotScroll(t *testing.T) {
	t.Parallel()
	f := NewFiles(newTestCommon(t))
	f.SetSize(80, 20)
	f.Update(FileContentMsg{content: strings.Repeat("line\n", 200), ext: ".txt"})
	f.code.GotoBottom()
	f.blameView = true
	before := f.code.YOffset()
	f.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if f.blameView {
		t.Fatal("b did not toggle the blame view off")
	}
	if got := f.code.YOffset(); got != before {
		t.Errorf("b scrolled the file from line %d to %d", before, got)
	}
	for _, k := range []tea.KeyPressMsg{
		{Code: 'b', Text: "b"},
		{Code: 'l', Text: "l"},
	} {
		if key.Matches(k, f.code.KeyMap.PageUp, f.code.KeyMap.Right) {
			t.Errorf("%q is still a viewport key in the file view", k.String())
		}
	}
}

// The commit list has nothing to go back to, so its help does not offer it.
func TestLogCommitListHelpHasNoBack(t *testing.T) {
	t.Parallel()
	l := NewLog(newTestCommon(t))
	l.SetSize(80, 20)
	for _, group := range l.FullHelp() {
		for _, k := range helpKeys(group) {
			if strings.Contains(k, "back") {
				t.Errorf("commit list help shows %q", k)
			}
		}
	}
}

// esc goes back to the repository menu only at the top of a tab, and there is
// no menu in soft browse.
func TestRepoBackHelp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		noMenu bool
		inDiff bool
		want   string
	}{
		{name: "menu", want: "esc back to menu"},
		{name: "sub-view", inDiff: true, want: "esc back"},
		{name: "browse sub-view", noMenu: true, inDiff: true, want: "esc back"},
		{name: "browse top", noMenu: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCommon(t)
			l := NewLog(c)
			if tc.inDiff {
				l.activeView = logViewDiff
			}
			r := New(c, l)
			r.SetNoMenu(tc.noMenu)
			var got string
			for _, k := range helpKeys(r.ShortHelp()) {
				if strings.HasPrefix(k, "esc ") {
					got = k
				}
			}
			if got != tc.want {
				t.Errorf("esc help = %q, want %q", got, tc.want)
			}
		})
	}
}

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

// deliver hands msgs to the page and then every message their commands
// produce, like settle, but lets errors through to the page as the program
// would. It returns how many errors reached the top, for the error screen.
func (p *pinPage) deliver(msgs ...tea.Msg) (errs int) {
	p.t.Helper()
	for steps := 0; len(msgs) > 0; steps++ {
		if steps > 10000 {
			p.t.Fatal("page never settles")
		}
		var msg tea.Msg
		msg, msgs = msgs[0], msgs[1:]
		switch msg.(type) {
		case spinner.TickMsg:
			continue
		case common.ErrorMsg:
			errs++
		}
		msgs = append(msgs, p.update(msg)...)
	}
	return errs
}

// A listing that arrives after a file was opened is dropped; it does
// not put the list back while the path names the file.
func TestFilesLateListingAfterFileOpened(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)

	// "h" at the top lists the directory again; its listing is ready but
	// still queued when the user opens a file.
	late := nonTicks(p.update(tea.KeyPressMsg{Code: 'h', Text: "h"}))
	if len(late) == 0 {
		t.Fatal("h started no listing")
	}
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	p.wantView("file", "file-body-two")

	p.settle(late...)
	if got := p.files.Path(); got != "file.txt" {
		t.Fatalf("path = %q after the late listing; want file.txt", got)
	}
	p.wantView("file after the late listing", "file-body-two")
	p.notView("file after the late listing", "sub")

	// Going back lists the directory the file is in.
	p.settle(keyEsc)
	if got := p.files.Path(); got != "" {
		t.Fatalf("path = %q after going back; want the top", got)
	}
	p.wantView("back to the list", "README.md", "file.txt", "sub")
}

// When opening repository B fails, the page shows nothing of the
// repository shown before under B's name, and takes no input meant for it.
func TestRepoFailedOpenHidesPreviousRepository(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	// A's Files tab is in a directory.
	p.tab(1)
	selectItem(t, p.files.selector, "sub")
	p.settle(keyEnter)
	if got := p.files.Path(); got != "sub" {
		t.Fatalf("files path = %q; want sub", got)
	}

	b := diskRepo{name: "broken-b", path: filepath.Join(t.TempDir(), "missing")}
	if errs := p.deliver(OpenedMsg(b)); errs == 0 {
		t.Fatal("the failed open reported no error")
	}
	p.wantView("failed repository", "broken-b")
	p.notView("failed repository", "readme-body", "inner.txt", "README.md", "loading", "Commits")

	// The tab key switches no tab, and help offers no tab's keys.
	p.deliver(keyTab)
	if p.r.activeTab != 0 {
		t.Fatalf("tab switched to tab %d on the failed repository", p.r.activeTab)
	}
	if n := len(p.r.ShortHelp()); n != len(p.r.commonHelp()) || n > 1 {
		t.Fatalf("help offers %d keys; want esc alone", n)
	}

	// A tab switch already on its way lands on A's Files tab, in sub.
	// The page still has no path, so esc leaves it, and enter does not
	// reach the tab.
	p.deliver(tabs.ActiveTabMsg(1))
	if got := p.r.Path(); got != "" {
		t.Fatalf("path = %q; want none, so esc leaves the page", got)
	}
	selectItem(t, p.files.selector, "inner.txt")
	for _, k := range []tea.KeyPressMsg{keyEnter, {Code: 'h', Text: "h"}} {
		p.deliver(k)
		if got := p.files.Path(); got != "sub" {
			t.Fatalf("%s changed A's Files tab to %q", k, got)
		}
	}
	p.notView("enter on the failed repository", "inner-body")
	if n := len(p.r.FullHelp()); n != 1 {
		t.Fatalf("full help has %d groups; want esc alone", n)
	}

	// Opening a repository that loads brings the tabs and their help back.
	p.settle(OpenedMsg(p.repo))
	p.wantView("repository A again", "Commits", "readme-body")
	if n := len(p.r.ShortHelp()); n <= len(p.r.commonHelp()) {
		t.Fatalf("help offers %d keys after A opened again; want the tab's too", n)
	}
	p.settle(keyTab)
	if p.r.activeTab != 1 {
		t.Fatalf("tab went to tab %d after A opened again; want 1", p.r.activeTab)
	}
}

// A file opened from the listing on screen, while another directory loads,
// is named after the directory it was listed in.
func TestFilesFileFromPreviousListingNamedRight(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)
	selectItem(t, p.files.selector, "sub")
	// sub is asked for; its listing is still on its way.
	var late []tea.Msg
	for _, m := range nonTicks(p.update(keyEnter)) {
		late = append(late, nonTicks(p.update(m))...)
	}
	if p.files.Path() != "sub" {
		t.Fatalf("path = %q; want sub", p.files.Path())
	}
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	p.wantView("root file", "file-body-two")
	p.settle(late...)
	if got := p.files.Path(); got != "file.txt" {
		t.Fatalf("the root file opened as %q; want file.txt", got)
	}
	// The back stack holds the file's position alone, so going back
	// lists the top with the cursor on the file.
	if got := p.files.lastSelected; len(got) != 1 {
		t.Fatalf("back stack = %v; want the file's position alone", got)
	}
	p.settle(keyEsc)
	if got := p.files.Path(); got != "" || len(p.files.lastSelected) != 0 {
		t.Fatalf("after esc path = %q, back stack = %v; want the top and nothing", got, p.files.lastSelected)
	}
	if it, ok := p.files.selector.SelectedItem().(FileItem); !ok || it.ID() != "file.txt" {
		t.Fatalf("after esc the cursor is on %v; want file.txt", p.files.selector.SelectedItem())
	}
}

// Opening a file stops a listing still on its way, so that the listing
// cannot fail afterwards and turn off a blame the user opened.
func TestFilesOpenedFileStopsListing(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)
	// "h" at the top lists the directory again; the user opens a file and
	// its blame before the listing runs.
	listing := p.files.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	selectItem(t, p.files.selector, "file.txt")
	p.settle(keyEnter)
	blame := nonTicks(p.update(keyB))

	// The listing then fails, the repository having gone away.
	if err := os.Rename(p.repo.path, p.repo.path+".gone"); err != nil {
		t.Fatal(err)
	}
	p.deliver(runCmd(listing)...)
	if !p.files.blameView {
		t.Fatal("a late listing turned off the blame the user opened")
	}
	p.deliver(blame...)
	p.wantView("blame", "second-subject", "file-body-two")
}

// A load that fails in a tab that is not shown reaches that tab, which
// leaves its loading view, rather than the tab that is shown.
func TestErrorReachesItsPane(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)

	// The repository goes away; a resize reloads the commit page, which
	// fails while the Files tab is shown.
	if err := os.Rename(p.repo.path, p.repo.path+".gone"); err != nil {
		t.Fatal(err)
	}
	p.r.SetSize(120, 30)
	if errs := p.deliver(tea.WindowSizeMsg{Width: 120, Height: 30}); errs == 0 {
		t.Fatal("the failed page load reported no error")
	}
	if _, loading := p.log.Loading(); loading {
		t.Fatal("the Commits tab is still loading after its load failed")
	}
}

// bidi are Unicode bidirectional controls: right-to-left override, left-to-
// right isolate and pop directional isolate.
const bidi = "\u202e\u2066\u2069"

// Commit subjects, authors and repository descriptions lose their
// bidirectional controls, as file names do, so they cannot reorder the text
// around them in a terminal that implements bidi.
func TestBidiControlsRemoved(t *testing.T) {
	t.Parallel()
	repo := newDiskRepo(t, "r", "main", "# r\n")
	gitRun(t, repo.path, "commit", "-q", "--allow-empty",
		"--author", "auth"+bidi+"or <a@b>", "-m", "sub"+bidi+"ject\n\nbo"+bidi+"dy")

	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	p := &pinPage{t: t, repo: repo, log: NewLog(c)}
	p.r = New(c, p.log)
	p.r.SetSize(120, 40)
	p.open()
	p.wantView("commit list", "subject", "author")

	p.r.selectedRepo = fakeRepo{projectName: "pro" + bidi + "ject", description: "des" + bidi + "cription"}
	p.settle(keyEnter)
	p.wantView("commit", "subject", "author", "body", "project", "description")
	if v := p.r.View(); strings.ContainsAny(v, bidi) {
		t.Fatalf("the page shows bidirectional controls:\n%q", ansi.Strip(v))
	}
	if strings.ContainsAny(p.log.StatusBarValue(), bidi) {
		t.Fatalf("the status bar value %q has bidirectional controls", p.log.StatusBarValue())
	}
}

// A submodule in the file list has nothing in this repository to show. Opening
// it says so, as a user error, rather than running git on a commit the
// repository does not have, which logged a server error per click.
func TestFilesSubmoduleIsUserError(t *testing.T) {
	t.Parallel()
	repo := newDiskRepo(t, "r", "main", "# r\n")
	gitRun(t, repo.path, "update-index", "--add", "--cacheinfo",
		"160000,0123456789abcdef0123456789abcdef01234567,mod")
	gitRun(t, repo.path, "commit", "-q", "-m", "add a submodule")

	ctx := context.Background()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(repo))
	ref, ok := updateRefCmd(t.Context(), repo, 1)().(RefMsg)
	if !ok {
		t.Fatal("updateRefCmd did not load the reference")
	}
	deliverAll(f, f.Update(ref))
	selectItem(t, f.selector, "mod")

	msg := findMsg[errorMsg](t, runCmd(f.Update(selector.SelectMsg{IdentifiableItem: f.selector.SelectedItem()})))
	if !common.IsUserError(msg.err) || common.ErrorMessage(msg.err) != "submodule" {
		t.Fatalf("opening a submodule reported %v (shown as %q); want the user error %q",
			msg.err, common.ErrorMessage(msg.err), "submodule")
	}
}

// Turning blame off on a Markdown file shows it rendered again, as it was
// before blame was turned on.
func TestFilesBlameOffRestoresMarkdown(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(1)
	selectItem(t, p.files.selector, "README.md")
	p.settle(keyEnter)
	if !p.files.code.UseGlamour {
		t.Fatal("the Markdown file is not rendered")
	}
	p.notView("rendered", "# Pin")

	p.settle(keyB)
	if p.files.code.UseGlamour {
		t.Fatal("blame shows the Markdown file rendered")
	}
	p.wantView("blame", "# Pin", "first-subject")

	p.settle(keyB)
	if !p.files.code.UseGlamour {
		t.Fatal("after blame is turned off, the Markdown file is shown as source")
	}
	p.wantView("blame off", "readme-body")
	p.notView("blame off", "# Pin", "first-subject")
}

// newReadmePage returns a repository page with only the Readme tab over repo.
func newReadmePage(t *testing.T, repo diskRepo) *pinPage {
	t.Helper()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	p := &pinPage{t: t, repo: repo, readme: NewReadme(c)}
	p.r = New(c, p.readme)
	p.r.SetSize(120, 40)
	return p
}

// A README larger than the size limit is reported as such, not as missing; a
// repository without one still shows that it has none.
func TestReadmeTooLarge(t *testing.T) {
	t.Parallel()
	big := newDiskRepo(t, "big", "main", "# big\n\n"+strings.Repeat("text ", config.MaxBlobSize/5+1)+"\n")
	p := newReadmePage(t, big)
	p.open()
	p.wantView("too large", "larger than the size limit")
	p.notView("too large", "No readme found.")

	none := newDiskRepo(t, "none", "main", "# none\n")
	gitRun(t, none.path, "rm", "-q", "README.md")
	gitRun(t, none.path, "commit", "-q", "-m", "no readme")
	p = newReadmePage(t, none)
	p.open()
	p.wantView("none", "No readme found.")

	// A root README too large to show gives way to a small one in .github.
	github := newDiskRepo(t, "github", "main", "# big\n\n"+strings.Repeat("text ", config.MaxBlobSize/5+1)+"\n")
	if err := os.MkdirAll(filepath.Join(github.path, ".github"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(github.path, ".github", "README.md"), "small-readme\n")
	gitRun(t, github.path, "add", "-A")
	gitRun(t, github.path, "commit", "-q", "-m", "small readme")
	p = newReadmePage(t, github)
	p.open()
	p.wantView(".github", "small-readme")
	p.notView(".github", "larger than the size limit")

	// With no README in the root, one too large in docs is reported.
	docs := newDiskRepo(t, "docs", "main", "# docs\n")
	gitRun(t, docs.path, "rm", "-q", "README.md")
	if err := os.MkdirAll(filepath.Join(docs.path, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(docs.path, "docs", "README.md"), strings.Repeat("text ", config.MaxBlobSize/5+1)+"\n")
	gitRun(t, docs.path, "add", "-A")
	gitRun(t, docs.path, "commit", "-q", "-m", "big readme in docs")
	p = newReadmePage(t, docs)
	p.open()
	p.wantView("docs", "larger than the size limit")
	p.notView("docs", "No readme found.")
}

// The file names in a diff's summary lose their bidirectional controls, as
// file names do everywhere else.
func TestDiffSummaryBidiControlsRemoved(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	out := renderSummary(&git.Diff{Stat: " na" + bidi + "me.txt | 1 +\n 1 file changed, 1 insertion(+)\n"}, c.Styles, 80)
	if strings.ContainsAny(out, bidi) {
		t.Fatalf("the diff summary keeps bidirectional controls: %q", out)
	}
	if !strings.Contains(ansi.Strip(out), "name.txt") {
		t.Fatalf("the diff summary lacks the file name: %q", ansi.Strip(out))
	}
}

// A result for a hidden tab leaves the status bar alone, so a copy notice
// stays until something on the tab shown changes.
func TestStatusBarKeepsNoticeOnHiddenTabResult(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(2)
	p.update(CopyMsg{Text: "x", Message: "Commit hash copied"})
	p.update(StashListMsg{gen: p.r.gen})
	if v := ansi.Strip(p.r.statusbar.View()); !strings.Contains(v, "Commit hash copied") {
		t.Fatalf("a hidden tab's result cleared the copy notice: %q", v)
	}
	p.update(LogCountMsg{count: 2, gen: p.r.gen})
	if v := ansi.Strip(p.r.statusbar.View()); strings.Contains(v, "Commit hash copied") {
		t.Fatalf("a result for the tab shown kept the copy notice: %q", v)
	}
}

// A README that fails to load for another reason reports the error, and
// does not leave the README of the ref shown before on screen.
func TestReadmeLoadErrorReported(t *testing.T) {
	t.Parallel()
	repo := newDiskRepo(t, "r", "main", "# r\n\nold-readme\n")
	p := newReadmePage(t, repo)
	p.open()
	p.wantView("open", "old-readme")

	// The repository goes away after its reference was read.
	ref, ok := updateRefCmd(t.Context(), repo, p.r.gen)().(RefMsg)
	if !ok {
		t.Fatal("updateRefCmd did not load the reference")
	}
	if err := os.Rename(repo.path, repo.path+".gone"); err != nil {
		t.Fatal(err)
	}
	var errs []errorMsg
	queue := p.update(ref)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			t.Fatal("page never settles")
		}
		msg := queue[0]
		queue = queue[1:]
		if m, ok := msg.(errorMsg); ok {
			errs = append(errs, m)
		}
		if _, ok := msg.(common.ErrorMsg); ok {
			continue
		}
		queue = append(queue, nonTicks(p.update(msg))...)
	}
	if len(errs) != 1 || errs[0].to != paneReadme {
		t.Fatalf("the failed README load reported %v; want one error for the Readme tab", errs)
	}
	p.notView("after the error", "old-readme")
}

// The status bar shows what a tab's load changed as soon as the result
// arrives, whichever result it is: here the commit count, which changes the
// number of pages.
func TestStatusBarRefreshedOnEveryResult(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	p.tab(2)
	if v := ansi.Strip(p.r.statusbar.View()); !strings.Contains(v, "p. 1/1") {
		t.Fatalf("status bar before the count: %q; want p. 1/1", v)
	}
	p.update(LogCountMsg{count: 1000, gen: p.r.gen})
	per := p.log.selector.PerPage()
	want := fmt.Sprintf("p. 1/%d", (1000+per-1)/per)
	if v := ansi.Strip(p.r.statusbar.View()); !strings.Contains(v, want) {
		t.Fatalf("status bar after the count: %q; want %s", v, want)
	}
}

// A code view render of a hidden tab leaves the status bar alone, so a copy
// notice stays; a render of the tab shown refreshes it.
func TestStatusBarKeepsNoticeOnHiddenTabRender(t *testing.T) {
	t.Parallel()
	p := newPinPage(t)
	p.open()
	notice := func() bool {
		return strings.Contains(ansi.Strip(p.r.statusbar.View()), "Commit hash copied")
	}
	render := func(c interface{ SetContent(string, string) tea.Cmd }) tea.Msg {
		t.Helper()
		return findMsg[code.RenderedMsg](t, runCmd(c.SetContent("body\n", "f.txt")))
	}

	p.tab(2)
	p.update(CopyMsg{Text: "x", Message: "Commit hash copied"})
	p.update(render(p.files.code))
	p.update(render(p.readme.code))
	if !notice() {
		t.Fatal("a hidden tab's render cleared the copy notice")
	}

	p.tab(1)
	p.update(CopyMsg{Text: "x", Message: "Commit hash copied"})
	p.update(render(p.files.code))
	if notice() {
		t.Fatal("a render of the tab shown kept the copy notice")
	}
}

// gatedRepo is a repository whose Open waits for gate while gating is set,
// so that loads stay in flight while the test sends more input, as they do
// when git is slow.
type gatedRepo struct {
	diskRepo
	gating  *atomic.Bool
	gate    chan struct{}
	waiting *atomic.Int32
}

func (r gatedRepo) Open(ctx context.Context) (*git.Repository, error) {
	if r.gating.Load() {
		r.waiting.Add(1)
		<-r.gate
	}
	return r.diskRepo.Open(ctx)
}

// gitLog puts a git on PATH that records each of its commands in a file
// before it runs the real git, and returns a function that counts the
// recorded commands that run sub, such as "ls-tree".
func gitLog(t *testing.T) func(sub string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recording git is a shell script")
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "git.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { // an executable script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(sub string) int {
		b, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(" "+line+" ", " "+sub+" ") {
				n++
			}
		}
		return n
	}
}

// loop is a stand-in for the bubbletea runtime: it runs each command in its
// own goroutine and hands its message back to the model, one at a time.
type loop struct {
	t       *testing.T
	m       interface{ Update(tea.Msg) tea.Cmd }
	results chan tea.Msg
	running atomic.Int32
}

func (l *loop) spawn(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	l.running.Add(1)
	go func() { l.results <- cmd() }()
}

// deliver hands msg to the model and spawns the commands it returns.
func (l *loop) deliver(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, cmd := range msg {
			l.spawn(cmd)
		}
	default:
		l.spawn(l.m.Update(msg))
	}
}

// settle delivers messages until every running command is waiting at the
// gate.
func (l *loop) settle(waiting *atomic.Int32) {
	if !uitest.WaitFor(func() bool {
		for {
			select {
			case msg := <-l.results:
				l.running.Add(-1)
				l.deliver(msg)
			default:
				return l.running.Load() <= waiting.Load()
			}
		}
	}) {
		l.t.Fatal("commands never settle")
	}
}

// 25 "l" and "h" pairs, sent faster than git answers, leave one load
// running: each new load cancels the one before, so the superseded ones
// start no git command. A resize loads nothing.
//
// Not parallel: it puts a recording git first on PATH.
func TestFilesOneLoadInFlight(t *testing.T) {
	count := gitLog(t)
	ctx := context.Background()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(120, 40)
	repo := gatedRepo{
		diskRepo: newPinRepo(t),
		gating:   new(atomic.Bool),
		gate:     make(chan struct{}),
		waiting:  new(atomic.Int32),
	}
	gr, err := repo.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{t: t, m: f, results: make(chan tea.Msg, 1000)}
	l.deliver(OpenedMsg(repo))
	l.deliver(RefMsg{Ref: ref})
	l.settle(repo.waiting)
	selectItem(t, f.selector, "file.txt")

	// A resize in the file list lists nothing again.
	before := count("ls-tree")
	l.deliver(tea.WindowSizeMsg{Width: 100, Height: 30})
	l.settle(repo.waiting)
	if n := count("ls-tree") - before; n != 0 {
		t.Fatalf("a resize listed the directory %d times; want none", n)
	}

	repo.gating.Store(true)
	before, files := count("ls-tree"), count("check-attr")
	for range 25 {
		l.deliver(tea.KeyPressMsg{Code: 'l', Text: "l"})
		l.settle(repo.waiting)
		l.deliver(tea.KeyPressMsg{Code: 'h', Text: "h"})
		l.settle(repo.waiting)
	}
	started := repo.waiting.Load()
	close(repo.gate)
	for l.running.Load() > 0 {
		if _, ok := uitest.Receive(l.results); !ok {
			t.Fatal("loads never finish")
		}
		l.running.Add(-1)
	}
	listings, opened := count("ls-tree")-before, count("check-attr")-files
	t.Logf("%d loads reached the repository; %d listings and %d file loads ran git", started, listings, opened)
	if started < 50 {
		t.Fatalf("only %d loads started; the test did not keep them in flight", started)
	}
	if listings+opened > 1 {
		t.Fatalf("%d listings and %d file loads ran git; want at most one load, the newest", listings, opened)
	}
	if cancelled := int(started) - listings - opened; cancelled < 49 {
		t.Fatalf("%d superseded loads were cancelled; want 49", cancelled)
	}
	if listings != 1 {
		t.Fatalf("the newest load, a listing, ran %d times; want once", listings)
	}
}

// Each resize or footer toggle reloads the commit page; a reload cancels the
// one before, whose command then sends nothing.
func TestLogPageLoadSupersedes(t *testing.T) {
	t.Parallel()
	l := newTestLog(t, filepath.Join(t.TempDir(), "repo"))
	first := resizeLog(l, 80, 30)
	second := resizeLog(l, 80, 20)
	for _, m := range runCmd(first) {
		if _, ok := m.(LogItemsMsg); ok {
			t.Fatal("a superseded page load ran to completion")
		}
	}
	findMsg[LogItemsMsg](t, runCmd(second))
}

// The commit view shows the author date, as git log does, not the
// committer date.
func TestRenderCommitShowsAuthorDate(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first", "--date", "2001-02-03T04:05:06Z")

	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CatFileCommit(head.ID())
	if err != nil {
		t.Fatal(err)
	}

	l := NewLog(uitest.NewCommon(t, context.Background(), nil, nil))
	l.SetSize(200, 40)
	out := l.renderCommit(c)
	if want := c.Author.When.Format(time.UnixDate); !strings.Contains(out, want) {
		t.Errorf("commit view %q lacks the author date %q", out, want)
	}
	if got := c.Committer.When.Format(time.UnixDate); strings.Contains(out, got) {
		t.Errorf("commit view %q shows the committer date %q", out, got)
	}
}

// References are sorted by the committer date they show, newest first.
func TestRefItemsSortByCommitterDate(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time {
		return time.Date(2020, 1, d, 0, 0, 0, 0, time.UTC)
	}
	// "old" was authored last but committed first.
	old := RefItem{&git.RefInfo{CommitID: "old", AuthorWhen: day(9), CommitterWhen: day(1)}}
	recent := RefItem{&git.RefInfo{CommitID: "recent", AuthorWhen: day(2), CommitterWhen: day(5)}}
	its := RefItems{old, recent}
	sort.Stable(its)
	if its[0].CommitID != recent.CommitID {
		t.Errorf("references sorted by author date: got committer dates %v, %v",
			its[0].CommitterWhen, its[1].CommitterWhen)
	}
}

// An empty directory shows no position rather than "# 1/0".
func TestFilesStatusBarInfoEmpty(t *testing.T) {
	t.Parallel()
	f := NewFiles(uitest.NewCommon(t, context.Background(), nil, nil))
	f.SetSize(80, 24)
	f.activeView = filesViewFiles
	if got := f.StatusBarInfo(); got != "# 0/0" {
		t.Errorf("StatusBarInfo for an empty directory = %q, want %q", got, "# 0/0")
	}
}

// The stash patch is highlighted once, by renderDiff, and shown as is.
//
// Not parallel: the server highlights at most a few files at once, and
// a parallel test holding the slots would leave this one unhighlighted.
func TestStashPatchHighlightedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	file := filepath.Join(dir, "main.go")
	writeFile(t, file, "package main\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	writeFile(t, file, "package main\n\nfunc main() {}\n")
	gitRun(t, dir, "stash", "-q")

	const width = 120
	s := NewStash(uitest.NewCommon(t, context.Background(), nil, nil))
	s.SetSize(width, 200)
	s.Update(OpenedMsg(openRepo{path: dir}))
	s.Update(s.fetchStashCmd()())
	msg, ok := s.fetchStashPatchCmd()().(StashPatchMsg)
	if !ok || msg.Diff == nil {
		t.Fatalf("fetchStashPatchCmd()() = %v, want a patch", msg)
	}
	updateRendered(s, msg)

	// Highlighting the patch again would wrap each line in another code
	// block, with its own margin and color, so it no longer starts as
	// renderDiff rendered it.
	view := strings.Split(s.View(), "\n")
	for _, line := range strings.Split(renderDiff(msg.Diff, width), "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}
		found := false
		for _, v := range view {
			if strings.HasPrefix(v, line) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("stash view has no line starting with the rendered diff line %q:\n%q", line, view)
		}
	}
}

// writeFile writes content to path and fails the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hostile holds the string sequences the bubbletea renderer passes through to
// the terminal: a DCS (DECRQSS, which makes the terminal answer), an APC
// (kitty graphics), an OSC 8 hyperlink with a hidden target, an OSC 52
// clipboard write, a C1 CSI encoded as UTF-8 and a BEL. It has no slash, so
// it can be part of a path in a git tree, but it holds control characters,
// ':' and '\', which Windows does not allow in a file name, so it never
// goes on disk.
const hostile = "\x1bP1$qm\x1b\\" +
	"\x1b_Gf=100;AAAA\x1b\\" +
	"\x1b]8;;https:evil.example\x1b\\link\x1b]8;;\x1b\\" +
	"\x1b]52;c;Zm9v\x07" +
	"\u009b2J" +
	"\x07"

// escapedRepo is a repository whose metadata and git data all carry hostile.
type escapedRepo struct {
	fakeRepo
	path string
}

func (r escapedRepo) Open(ctx context.Context) (*git.Repository, error) { return git.Open(ctx, r.path) }

// newEscapedRepo makes a repository where everything the TUI shows from git
// carries hostile: commit subjects and bodies, author names, a file name, a
// source file, a README, a tag message, a branch name, a tag name and a
// stash message. Git allows no ESC in a ref name, so the branch and tag
// names carry a C1 CSI, the only control git lets through.
func newEscapedRepo(t *testing.T) escapedRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	files := map[string]string{
		"README.md": "# Readme" + hostile + "\n\nreadme-text" + hostile + "\n",
		"main.go":   "package main\n\n// go-comment" + hostile + "\nfunc main() {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	// The hostile file name goes straight into the index, so no file of that
	// name is made; core.protectNTFS would reject the '\' in it on Windows.
	// No git command below takes the name as a path argument: Git for
	// Windows turns the '\' in such an argument into '/' and then cannot
	// find the entry.
	hostileName := "name" + hostile + "end.txt"
	blob := gitStdin(t, dir, "plain-text\n", "hash-object", "-w", "--stdin")
	gitRun(t, dir, "-c", "core.protectNTFS=false", "update-index", "--add",
		"--cacheinfo", "100644,"+blob+","+hostileName)
	gitRun(t, dir, "-c", "user.name=eve-author"+hostile,
		"commit", "-q", "--author", "eve-author"+hostile+" <eve@example.com>",
		"-m", "commit-subject"+hostile+"\n\ncommit-body"+hostile)
	gitRun(t, dir, "tag", "-a", "v1", "-m", "tag-message"+hostile)
	gitRun(t, dir, "branch", "branch\u009b2Jname")
	gitRun(t, dir, "tag", "tag\u009b2Jname")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stash is limited to main.go, so it neither records the missing
	// hostile file as deleted nor checks it out when it resets the tree.
	gitRun(t, dir, "stash", "push", "-q", "-m", "stash-message"+hostile, "--", "main.go")
	// Windows rejects the name itself (ERROR_INVALID_NAME) rather than
	// reporting it missing; either way no such file is on disk.
	if _, err := os.Lstat(filepath.Join(dir, hostileName)); err == nil ||
		(!os.IsNotExist(err) && runtime.GOOS != "windows") {
		t.Fatalf("hostile file name reached the disk: %v", err)
	}
	return escapedRepo{
		fakeRepo: fakeRepo{
			projectName: "project-name" + hostile,
			description: "description" + hostile,
		},
		path: dir,
	}
}

// gitStdin runs git in dir with stdin as its input and returns its output.
func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// driver feeds messages to a model and runs the commands it returns, the way
// the bubbletea runtime would, until no more messages come.
type driver struct {
	t *testing.T
	m common.Model
}

// cmdType lets send unpack the unexported message tea.Sequence returns, a
// slice of commands like tea.BatchMsg.
var cmdType = reflect.TypeOf((*tea.Cmd)(nil)).Elem()

func (d *driver) send(msg tea.Msg) {
	d.t.Helper()
	queue := []tea.Msg{msg}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 10000 {
			d.t.Fatal("model never settles")
		}
		msg, queue = queue[0], queue[1:]
		switch msg := msg.(type) {
		case nil, spinner.TickMsg:
			// Spinners tick forever; nothing a test checks depends on them.
			continue
		case common.ErrorMsg:
			d.t.Fatalf("model reported an error: %v", msg.Err)
		case errorMsg:
			d.t.Fatalf("model reported an error: %v", msg.err)
		case tea.BatchMsg:
			for _, cmd := range msg {
				queue = append(queue, d.run(cmd))
			}
			continue
		}
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
			for i := range v.Len() {
				queue = append(queue, d.run(v.Index(i).Interface().(tea.Cmd)))
			}
			continue
		}
		queue = append(queue, d.run(d.m.Update(msg)))
	}
}

// run runs cmd and returns its message, or nil if it has none. It waits for
// the command however slow the runner is, so no message is lost; the only
// timers the panes start are spinner ticks, which send drops by type.
func (d *driver) run(cmd tea.Cmd) tea.Msg {
	d.t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	msg, ok := uitest.Receive(ch)
	if !ok {
		d.t.Fatal("command did not finish")
	}
	return msg
}

// Every pane of the repository page, and the header and status bar around
// it, shows data from the repository. Whatever a repository carries, none of
// it may reach the terminal as an escape sequence or control character, while
// the text around the sequences stays visible.
func TestEveryPaneSanitisesRepositoryData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	rr := newEscapedRepo(t)

	readme := NewReadme(c)
	readme.SetSize(200, 60)
	files := NewFiles(c)
	files.SetSize(200, 60)
	log := NewLog(c)
	log.SetSize(200, 60)
	branches := NewRefs(c, git.RefsHeads)
	branches.SetSize(200, 60)
	tags := NewRefs(c, git.RefsTags)
	tags.SetSize(200, 60)
	stash := NewStash(c)
	stash.SetSize(200, 60)
	r := New(c, readme, files, log, branches, tags, stash)
	r.SetSize(200, 60)
	d := &driver{t: t, m: r}

	d.send(OpenedMsg(rr))
	d.send(updateRefCmd(ctx, rr, r.gen)())

	check := func(name string, want ...string) {
		t.Helper()
		out := d.m.View()
		assertClean(t, out, append([]string{"project-name", "description"}, want...)...)
		if t.Failed() {
			t.Fatalf("%s: view is not clean", name)
		}
	}
	tab := func(p Pane) {
		t.Helper()
		for i, q := range r.panes {
			if q == p {
				d.send(tabs.SelectTabMsg(i))
				return
			}
		}
		t.Fatalf("no tab %s", p.TabName())
	}

	tab(readme)
	check("readme", "readme-text")

	tab(files)
	check("file list", "name\\x1bP1$qm")
	for _, it := range files.selector.Items() {
		fi := it.(FileItem)
		if strings.HasPrefix(fi.entry.Name(), "name") {
			// ls-tree quotes a name that holds control characters, and the
			// Files pane passes the quoted name back to git, which cannot
			// find it; the name is covered by the list above.
			continue
		}
		d.send(selector.SelectMsg{IdentifiableItem: fi})
		switch fi.entry.Name() {
		case "main.go":
			check("source file", "go-comment")
		case "README.md":
			check("markdown file", "readme-text")
		}
		d.send(tea.KeyPressMsg{Code: 'b', Text: "b"})
		check("blame "+fi.entry.Name(), "commit-subject", "eve-author")
		d.send(tea.KeyPressMsg{Code: 'b', Text: "b"})
		d.send(GoBackMsg{})
	}

	tab(log)
	check("commit list", "commit-subject", "eve-author")
	d.send(selector.SelectMsg{IdentifiableItem: log.selector.SelectedItem()})
	check("commit", "commit-subject", "commit-body", "eve-author", "plain-text", "go-comment")
	d.send(GoBackMsg{})

	tab(branches)
	for i := range branches.selector.Items() {
		branches.selector.Select(i)
		d.send(selector.ActiveMsg{IdentifiableItem: branches.selector.SelectedItem()})
		check("branches", "branch2Jname")
	}

	tab(tags)
	check("tags", "tag-message", "v1", "tag2Jname")

	tab(stash)
	check("stash list", "stash-message")
	d.send(selector.SelectMsg{IdentifiableItem: stash.list.SelectedItem()})
	check("stash patch", "stash-message", "go-comment")
}

// Opening a file from the listing shown while another directory loads does
// not cancel that listing: when the file fails to open, the pane still shows
// the directory it is in.
func TestFilesFileLoadKeepsListing(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub", "inner.txt"), "inner\n")
	writeFile(t, filepath.Join(dir, "bin.dat"), "\x00\x01\x02binary")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	gr, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.Update(OpenedMsg(openRepo{path: dir}))
	f.Update(findMsg[FileItemsMsg](t, runCmd(f.Update(RefMsg{Ref: ref}))))
	item := func(name string) selector.SelectMsg {
		for _, it := range f.selector.Items() {
			if it.(selector.IdentifiableItem).ID() == name {
				return selector.SelectMsg{IdentifiableItem: it.(selector.IdentifiableItem)}
			}
		}
		t.Fatalf("no item %q", name)
		return selector.SelectMsg{}
	}

	// Enter sub, then open bin.dat from the root listing still on screen,
	// before sub is listed.
	listing := f.Update(item("sub"))
	file := f.Update(item("bin.dat"))
	for _, m := range runCmd(file) {
		f.Update(m)
	}
	for _, m := range runCmd(listing) {
		f.Update(m)
	}
	var names []string
	for _, it := range f.selector.Items() {
		names = append(names, it.(selector.IdentifiableItem).ID())
	}
	if f.Path() != "sub" || len(names) != 1 || names[0] != "inner.txt" {
		t.Fatalf("path %q lists %v; want sub listing inner.txt", f.Path(), names)
	}
}

// A load is cancelled when its pane resets, so a late result cannot switch
// the pane's view or do work for nothing.
func TestPaneResetCancelsLoads(t *testing.T) {
	t.Parallel()
	stashItem := func(p *pinPage) selector.SelectMsg {
		return selector.SelectMsg{IdentifiableItem: p.stash.list.SelectedItem()}
	}
	cases := map[string]func(p *pinPage) tea.Cmd{
		"stash patch, back": func(p *pinPage) tea.Cmd {
			cmd := p.stash.Update(stashItem(p))
			p.stash.Update(GoBackMsg{})
			return cmd
		},
		"stash patch, new ref": func(p *pinPage) tea.Cmd {
			cmd := p.stash.Update(stashItem(p))
			p.stash.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log diff, new ref": func(p *pinPage) tea.Cmd {
			sel := p.log.selector.SelectedItem()
			cmd := p.log.Update(selector.SelectMsg{IdentifiableItem: sel})
			p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log page, new ref": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			return cmd
		},
		"log count, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(RefMsg{Ref: p.r.ref, gen: p.r.gen})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"log diff, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(selector.SelectMsg{IdentifiableItem: p.log.selector.SelectedItem()})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"log page, empty": func(p *pinPage) tea.Cmd {
			cmd := p.log.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			p.log.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"readme, empty": func(p *pinPage) tea.Cmd {
			cmd := p.readme.Init()
			p.readme.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
		"refs, empty": func(p *pinPage) tea.Cmd {
			cmd := p.branches.Init()
			p.branches.Update(EmptyRepoMsg{gen: p.r.gen})
			return cmd
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newPinPage(t)
			p.open()
			noLoadResult(t, runCmd(start(p)))
		})
	}
}

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
		r.Update(OpenedMsg(fakeRepo{}))
		r.state = readyState

		// Zones are stored in the order they are marked, so once the
		// sentinel after the page is stored, so is every zone of the page.
		c.Zone.Scan(r.View() + c.Zone.Mark("sentinel", "x"))
		if !uitest.WaitFor(func() bool { return !c.Zone.Get("sentinel").IsZero() }) {
			t.Fatal("the zones were never stored")
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
		"Readme":       func() size { return size{p.readme.code.Width(), p.readme.code.Height()} },
		"Files":        func() size { return size{p.files.selector.Width(), p.files.selector.Height()} },
		"Files code":   func() size { return size{p.files.code.Width(), p.files.code.Height()} },
		"Commits":      func() size { return size{p.log.selector.Width(), p.log.selector.Height()} },
		"Commits diff": func() size { return size{p.log.vp.Width(), p.log.vp.Height()} },
		"Stash":        func() size { return size{p.stash.list.Width(), p.stash.list.Height()} },
		"Stash patch":  func() size { return size{p.stash.vp.Width(), p.stash.vp.Height()} },
		"Branches":     func() size { return size{p.branches.selector.Width(), p.branches.selector.Height()} },
		"Tags":         func() size { return size{p.tags.selector.Width(), p.tags.selector.Height()} },
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

// countingPane is a tab that counts the messages it receives by type.
type countingPane struct {
	pane paneID
	name string
	got  map[string]int
}

func newCountingPane(id paneID, name string) *countingPane {
	return &countingPane{pane: id, name: name, got: map[string]int{}}
}

func (p *countingPane) Init() tea.Cmd { return nil }
func (p *countingPane) Update(msg tea.Msg) tea.Cmd {
	p.got[fmt.Sprintf("%T", msg)]++
	return nil
}
func (p *countingPane) View() string              { return "" }
func (p *countingPane) ShortHelp() []key.Binding  { return nil }
func (p *countingPane) FullHelp() [][]key.Binding { return nil }
func (p *countingPane) SetSize(int, int)          {}
func (p *countingPane) StatusBarValue() string    { return "" }
func (p *countingPane) StatusBarInfo() string     { return "" }
func (p *countingPane) Loading() (string, bool)   { return "", false }
func (p *countingPane) id() paneID                { return p.pane }
func (p *countingPane) TabName() string           { return p.name }
func (p *countingPane) Path() string              { return "" }
func (p *countingPane) count(msg tea.Msg) int     { return p.got[fmt.Sprintf("%T", msg)] }

var _ Pane = (*countingPane)(nil)

// Every message reaches each pane it is meant for exactly once, the active
// pane included.
func TestRepoDeliversMessagesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	readme := newCountingPane(paneReadme, "Readme")
	files := newCountingPane(paneFiles, "Files")
	log := newCountingPane(paneLog, "Commits")
	r := New(c, readme, files, log)
	r.SetSize(80, 40)

	// Each message is built for the generation that is current when it is
	// sent, since the repository page drops messages from an older one.
	for _, tc := range []struct {
		msg  func(gen uint64) tea.Msg
		want map[*countingPane]int
	}{
		{func(uint64) tea.Msg { return tea.WindowSizeMsg{Width: 100, Height: 50} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(uint64) tea.Msg { return OpenedMsg(fakeRepo{}) }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return RefMsg{gen: g} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return EmptyRepoMsg{gen: g} }, map[*countingPane]int{readme: 1, files: 1, log: 1}},
		{func(g uint64) tea.Msg { return ReadmeMsg{gen: g} }, map[*countingPane]int{readme: 1}},
		{func(g uint64) tea.Msg { return FileContentMsg{gen: g} }, map[*countingPane]int{files: 1}},
		{func(g uint64) tea.Msg { return LogCountMsg{count: 1, gen: g} }, map[*countingPane]int{log: 1}},
		{func(g uint64) tea.Msg { return FileBlameMsg{gen: g} }, map[*countingPane]int{files: 1}},
		{func(g uint64) tea.Msg { return LogDiffMsg{gen: g} }, map[*countingPane]int{log: 1}},
		{func(uint64) tea.Msg { return spinner.TickMsg{ID: -2} }, map[*countingPane]int{}},
	} {
		// Each tab takes a turn as the active one, so that the tab a message is
		// addressed to is active as well as inactive.
		for a := range r.panes {
			r.activeTab = a
			msg := tc.msg(r.gen)
			before := map[*countingPane]int{}
			for _, p := range []*countingPane{readme, files, log} {
				before[p] = p.count(msg)
			}
			r.Update(msg)
			for _, p := range []*countingPane{readme, files, log} {
				if got := p.count(msg) - before[p]; got != tc.want[p] {
					t.Errorf("%T with tab %d active: pane %q got it %d times, want %d",
						msg, a, p.name, got, tc.want[p])
				}
			}
		}
	}
}

// A resize reaches the code view of the Files tab once while a file is open.
func TestFilesDeliversResizeToContentOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFiles(uitest.NewCommon(t, ctx, nil, nil))
	f.SetSize(80, 40)
	f.activeView = filesViewContent
	f.currentContent = FileContentMsg{content: "hello", ext: ".txt"}
	f.code.SetContent("hello", ".txt")

	before := f.code.Resizes()
	f.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	if got := f.code.Resizes() - before; got != 1 {
		t.Errorf("code view got the resize %d times, want 1", got)
	}
}

// escapes holds sequences the bubbletea renderer would pass to the terminal:
// a DCS, an APC, an OSC 52 clipboard write, a C1 CSI and a BEL.
const escapes = "\x1bP1|x\x1b\\\x1b_Gx\x1b\\\x1b]52;c;Zm9v\x07\u009b2J\x07"

// csi matches the CSI sequences the TUI itself emits (SGR styles and zone
// marks); anything else that controls the terminal must come from data.
var csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// assertClean fails if out, less the TUI's own CSI sequences, holds a
// control character other than newline and tab, or lacks any of want.
func assertClean(t *testing.T, out string, want ...string) {
	t.Helper()
	rest := csi.ReplaceAllString(out, "")
	for _, r := range rest {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
			t.Fatalf("output holds control character %q: %q", r, out)
		}
	}
	for _, w := range want {
		if !strings.Contains(rest, w) {
			t.Errorf("output lacks %q: %q", w, rest)
		}
	}
}

func escapedCommit() *git.Commit {
	sig := &gitm.Signature{Name: "eve" + escapes, Email: "eve@example.com" + escapes, When: time.Now()}
	return &git.Commit{
		ID:        gitm.MustIDFromString("0123456789abcdef0123456789abcdef01234567"),
		Author:    sig,
		Committer: sig,
		Message:   "subject" + escapes + "\r\n\r\nbody" + escapes + "\r\n",
	}
}

func TestHeaderSanitisesProjectNameAndDescription(t *testing.T) {
	t.Parallel()
	r := &Repo{
		common:       uitest.NewCommon(t, context.Background(), nil, nil),
		width:        120,
		height:       24,
		selectedRepo: fakeRepo{projectName: "proj" + escapes, description: "desc" + escapes},
	}
	assertClean(t, r.headerView(), "proj", "desc")
}

func TestLogSanitisesCommit(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, context.Background(), nil, nil)
	commit := escapedCommit()

	l := &Log{common: c, width: 120, activeView: logViewCommits, activeCommit: commit}
	assertClean(t, l.renderCommit(commit), "subject", "body", "eve")
	assertClean(t, l.StatusBarValue(), "eve")

	item := LogItem{commit}
	d := LogItemDelegate{c}
	m := list.New([]list.Item{item}, d, 120, 24)
	var buf bytes.Buffer
	d.Render(&buf, m, 0, item)
	assertClean(t, buf.String(), "subject", "eve")
}

func TestStashItemSanitisesMessage(t *testing.T) {
	t.Parallel()
	item := StashItem{&gitm.Stash{Message: "stash" + escapes}}
	assertClean(t, item.Title(), "stash")
}

// File contents and READMEs reach the terminal through chroma and glamour,
// which keep DCS and APC strings.
func TestCodeSanitisesContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)

	f := NewFiles(c)
	f.SetSize(120, 40)
	updateRendered(f, FileContentMsg{content: "package main\n\n// comment" + escapes + "\nfunc main() {}\n", ext: "main.go", load: f.load})
	assertClean(t, f.code.View(), "comment")
	// The raw bytes are kept for copying to the clipboard.
	if !strings.Contains(f.currentContent.content, escapes) {
		t.Errorf("file content for copying was altered: %q", f.currentContent.content)
	}

	r := NewReadme(c)
	r.SetSize(120, 40)
	updateRendered(r, ReadmeMsg{Content: "# Title" + escapes + "\n\ntext" + escapes + "\n", Path: "README.md"})
	assertClean(t, r.code.View(), "text")
}

// A stray 8-bit byte from a legacy-encoded file, or an unterminated string
// sequence, must not hide the lines after it.
func TestCodeKeepsContentAfterBrokenSequences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	for _, content := range []string{
		"line1 na\x9eve\nline2 VISIBLE_AFTER_C1\n",
		"line1 \x90\nline2 VISIBLE_AFTER_C1\n",
		"line1\nhidden \x1bP start\nline3 MALICIOUS_CODE\n",
		"line1\nhidden \x1b]0;title\nline3 MALICIOUS_CODE\n",
	} {
		f := NewFiles(c)
		f.SetSize(120, 40)
		updateRendered(f, FileContentMsg{content: content, ext: "file.txt", load: f.load})
		want := "MALICIOUS_CODE"
		if strings.Contains(content, "VISIBLE_AFTER_C1") {
			want = "VISIBLE_AFTER_C1"
		}
		assertClean(t, f.code.View(), want)
	}
}

// git's stat for a binary file ("Bin 2 -> 3 bytes") has no +/- graph; the
// summary must not colour the "-" of its "->" as a deletion. A text file's
// graph still is coloured.
func TestRenderSummaryBinaryStat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)
	del := c.Styles.Log.CommitStatsDel.Render("-")
	if del == "-" {
		t.Skip("the renderer adds no styling")
	}
	out := renderSummary(&git.Diff{Stat: " b.bin | Bin 2 -> 3 bytes\n t.txt | 2 +-\n"}, c.Styles, 120)
	lines := strings.Split(out, "\n")
	if strings.Contains(lines[0], del) || !strings.Contains(lines[0], "Bin 2 -> 3 bytes") {
		t.Errorf("binary stat line = %q, want it unstyled", lines[0])
	}
	if !strings.Contains(lines[1], del) {
		t.Errorf("text stat line = %q, want its - styled", lines[1])
	}
}

// The stash patch view is built from pieces the TUI has already styled
// (and sanitised at their source); the code view must keep those styles.
func TestStashPatchKeepsStyling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := uitest.NewCommon(t, ctx, nil, nil)

	diff := &git.Diff{
		Stat: " a.txt | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n",
		Patch: "diff --git a/a.txt b/a.txt\n" +
			"index 0123456..789abcd 100644\n" +
			"--- a/a.txt\n" +
			"+++ b/a.txt\n" +
			"@@ -1 +1 @@\n" +
			"-old\n" +
			"+new\n",
	}

	s := NewStash(c)
	s.SetSize(120, 40)
	s.Update(StashListMsg{stashes: []*gitm.Stash{{Message: "stash title"}}})
	updateRendered(s, StashPatchMsg{Diff: diff})
	out := s.vp.View()
	for _, want := range []string{
		c.Styles.Stash.Title.Render("stash title"),
		c.Styles.Log.CommitStatsAdd.Render("+"),
		c.Styles.Log.CommitStatsDel.Render("-"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stash patch view lacks styled %q: %q", want, out)
		}
	}
}

// The repository page turns one spinner: it stops once nothing loads, one
// tick chain starts again when a load begins, and a tick that was already
// handled is dropped.
func TestRepoSpinnerLifecycle(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := newPinPage(t)
	p.open()

	p.log.waitBeforeLoading = time.Hour
	p.log.startLoading()
	if text, ok := p.log.Loading(); text != "" || !ok {
		t.Fatalf("Loading() before the wait = (%q, %v); want (\"\", true)", text, ok)
	}
	p.r.Update(selectTab(2))
	p.wantView("commits before the wait", "second-subject")
	p.notView("commits before the wait", "loading")

	// Negative, not zero: see newPinPage.
	p.log.waitBeforeLoading = -time.Hour
	if text, ok := p.log.Loading(); text != "loading commits…" || !ok {
		t.Fatalf("Loading() after the wait = (%q, %v); want (\"loading commits…\", true)", text, ok)
	}
	p.wantView("commits after the wait", "loading commits…")
}
