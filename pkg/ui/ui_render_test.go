package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/code"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/tabs"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/charmbracelet/x/ansi"
)

// commitTo pushes a commit that adds a file to the server repository name.
func commitTo(t *testing.T, ui *Model, name string) {
	t.Helper()
	r, err := ui.common.Backend.Repository(ui.common.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	gr, err := r.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("diff-body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "the-subject")
	run("push", "-q", gr.Path, "main")
}

// settleUI delivers msgs to ui and every message their commands produce,
// until none is left, dropping spinner ticks.
func settleUI(t *testing.T, ui *Model, msgs ...tea.Msg) {
	t.Helper()
	for steps := 0; len(msgs) > 0; steps++ {
		if steps > 10000 {
			t.Fatal("UI never settles")
		}
		msg := msgs[0]
		msgs = msgs[1:]
		if _, ok := msg.(spinner.TickMsg); ok {
			continue
		}
		_, cmd := ui.Update(msg)
		msgs = append(msgs, runCmd(cmd)...)
	}
}

// openCommit opens repository "a" on its Commits tab and opens its commit.
func openCommit(t *testing.T, ui *Model) {
	t.Helper()
	settleUI(t, ui, ui.openRepoCmd("a")())
	settleUI(t, ui, tabs.SelectTabMsg(2))
	settleUI(t, ui, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// A diff render still running when the user leaves the repository page
// returns while the selection page is shown. It must reach the Commits tab,
// or the tab waits for it for ever and shows no diff again.
func TestUIDiffRenderAfterLeavingRepository(t *testing.T) {
	t.Parallel()
	ui := newRepoTestUI(t)
	commitTo(t, ui, "a")
	settleUI(t, ui, tea.WindowSizeMsg{Width: 80, Height: 40})
	openCommit(t, ui)
	if v := ansi.Strip(ui.View().Content); !strings.Contains(v, "diff-body") {
		t.Fatalf("the diff is not shown:\n%s", v)
	}

	// A width change starts a render; the user leaves before it returns.
	_, cmd := ui.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	var late []tea.Msg
	for _, m := range runCmd(cmd) {
		if _, ok := m.(repo.RenderedMsg); ok {
			late = append(late, m)
		}
	}
	if len(late) != 1 {
		t.Fatalf("the width change started %d renders; want 1", len(late))
	}
	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	settleUI(t, ui, esc)
	settleUI(t, ui, esc)
	if ui.activePage != selectionPage {
		t.Fatal("esc did not leave the repository page")
	}
	// The UI hands the render to the repository page, which is not shown.
	// This check is the part of the test that tests the routing. The end
	// to end half below passes even when the render is lost: opening a
	// commit resets the tab's render gate, so the diff opened again renders
	// whether or not the late render arrived.
	routing, shown := newTestUI(t, selectionPage)
	routing.Update(late[0])
	if got := routing.pages[repoPage].(*recordingPage).msgs; len(got) != 1 || len(shown.msgs) != 0 {
		t.Fatalf("the render reached the repository page %d times and the selection page %d times; want 1 and 0",
			len(got), len(shown.msgs))
	}
	settleUI(t, ui, late...)

	openCommit(t, ui)
	v := ansi.Strip(ui.View().Content)
	if strings.Contains(v, "loading commit") || !strings.Contains(v, "diff-body") {
		t.Fatalf("the diff opened after leaving the page is not shown:\n%s", v)
	}
}

// A code view renders in a command, and its result goes to every page: the
// server README on the selection page renders while the repository page is
// shown.
func TestUIDeliversRendersToEveryPage(t *testing.T) {
	t.Parallel()
	ui, active := newTestUI(t, repoPage)
	inactive := ui.pages[selectionPage].(*recordingPage)
	ui.Update(code.RenderedMsg{})
	if len(inactive.msgs) != 1 || len(active.msgs) != 1 {
		t.Fatalf("render delivered %d times to the selection page and %d times to the repository page; want once each",
			len(inactive.msgs), len(active.msgs))
	}
}
