package repo

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
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
	p.tab(1)
	p.wantView("repository A", "file.txt")

	b := diskRepo{name: "broken-b", path: filepath.Join(t.TempDir(), "missing")}
	if errs := p.deliver(RepoMsg(b)); errs == 0 {
		t.Fatal("the failed open reported no error")
	}
	p.wantView("failed repository", "broken-b")
	p.notView("failed repository", "readme-body", "file.txt", "README.md", "loading")
	if got := p.r.Path(); got != "" {
		t.Fatalf("path = %q; want none, so esc leaves the page", got)
	}

	// Input does not reach A's panes: enter opens nothing.
	p.deliver(keyEnter)
	p.notView("enter on the failed repository", "file-body-two")
	if got := p.files.Path(); got != "" {
		t.Fatalf("enter opened %q from the previous repository", got)
	}
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
