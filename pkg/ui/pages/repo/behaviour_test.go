package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/x/ansi"
)

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
	old := waitBeforeLoading
	waitBeforeLoading = -time.Hour
	t.Cleanup(func() { waitBeforeLoading = old })

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 120, 40)
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
			p.t.Fatalf("page reported an error: %v", msg)
		}
		msgs = append(msgs, p.update(msg)...)
	}
}

// open shows the repository and waits for every tab to load.
func (p *pinPage) open() {
	p.t.Helper()
	p.settle(RepoMsg(p.repo))
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
	p := newPinPage(t)
	p.r.Init()

	// The page loads the reference first.
	msgs := p.update(RepoMsg(p.repo))
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
