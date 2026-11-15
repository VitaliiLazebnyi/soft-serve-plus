package repo

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
)

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

// UI-6: a listing that arrives after a file was opened is dropped; it does
// not put the list back while the path names the file.
func TestFilesLateListingAfterFileOpened(t *testing.T) {
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

// UI-9: when opening repository B fails, the page shows nothing of the
// repository shown before under B's name, and takes no input meant for it.
func TestRepoFailedOpenHidesPreviousRepository(t *testing.T) {
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
	if errs := p.deliver(RepoMsg(b)); errs == 0 {
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
	p.settle(RepoMsg(p.repo))
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
	p := newPinPage(t)
	p.open()
	p.tab(1)

	// The repository goes away; a resize reloads the commit page, which
	// fails while the Files tab is shown.
	if err := os.Rename(p.repo.path, p.repo.path+".gone"); err != nil {
		t.Fatal(err)
	}
	if errs := p.deliver(tea.WindowSizeMsg{Width: 120, Height: 30}); errs == 0 {
		t.Fatal("the failed page load reported no error")
	}
	if _, loading := p.log.Loading(); loading {
		t.Fatal("the Commits tab is still loading after its load failed")
	}
}
