package browse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
	"github.com/charmbracelet/x/ansi"
)

func hasHelp(bindings []key.Binding, k string) bool {
	for _, b := range bindings {
		if b.Help().Key == k {
			return true
		}
	}
	return false
}

// msgs runs cmd to completion, expanding batches, and returns the messages
// it produces. Every command the model returns here finishes on its own
// (loading the reference runs git, which can take seconds on a loaded Windows
// runner), so each one is waited for rather than dropped after a deadline;
// the deadline below only turns a hang into a failure.
func msgs(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(uitest.HangGuard):
		t.Fatal("command did not return")
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, msgs(t, c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func hasRefMsg(ms []tea.Msg) bool {
	for _, msg := range ms {
		if _, ok := msg.(repo.RefMsg); ok {
			return true
		}
	}
	return false
}

// The reference is loaded only after OpenedMsg is handled: batched commands run
// concurrently, so a RefMsg sent alongside OpenedMsg could reach the tabs before
// they have a repository to read from.
func TestRefAfterRepo(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	r, err := git.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	m, err := newModel(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	started := msgs(t, m.Init())
	if hasRefMsg(started) {
		t.Fatal("Init sends a RefMsg alongside OpenedMsg")
	}
	var repoMsg tea.Msg
	for _, msg := range started {
		if _, ok := msg.(repo.OpenedMsg); ok {
			repoMsg = msg
		}
	}
	if repoMsg == nil {
		t.Fatalf("Init sends no OpenedMsg: %v", started)
	}

	_, cmd := m.Update(repoMsg)
	if got := msgs(t, cmd); !hasRefMsg(got) {
		t.Fatalf("handling OpenedMsg does not load the reference: %v", got)
	}
}

// TestQuitKeepsZoneManagerRunning checks that quitting leaves the zone
// manager running for the program's last render, which blocks forever after
// the manager is closed once the view has more zones than its worker buffers.
func TestQuitKeepsZoneManagerRunning(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	m := &model{common: c, width: 80, height: 24}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("quit key returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("quit key did not quit")
	}

	var view strings.Builder
	for i := range 500 {
		view.WriteString(c.Zone.Mark(fmt.Sprintf("zone-%d", i), "x"))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Zone.Scan(view.String())
	}()
	select {
	case <-done:
	case <-time.After(uitest.HangGuard):
		t.Fatal("render after quit blocked: the zone manager was closed on quit")
	}
}

// The help footer of soft browse shows how to quit and toggle help.
func TestBrowseHelpShowsQuitAndHelp(t *testing.T) {
	t.Parallel()
	c := uitest.NewCommon(t, t.Context(), nil, nil)
	m := &model{model: repo.New(c, repo.NewLog(c)), common: c, width: 80, height: 24}
	short := m.ShortHelp()
	var full []key.Binding
	for _, g := range m.FullHelp() {
		full = append(full, g...)
	}
	for _, k := range []string{"q", "?"} {
		if !hasHelp(short, k) {
			t.Errorf("short help lacks %q", k)
		}
		if !hasHelp(full, k) {
			t.Errorf("full help lacks %q", k)
		}
	}
}

// The error screen sanitises the error, which can carry bytes from the
// repository: no escape sequence or bidirectional control reaches the
// terminal.
func TestBrowseErrorSanitised(t *testing.T) {
	t.Parallel()
	m, err := newModel(t.Context(), newDriveRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.common.Zone.Close)
	m.SetSize(80, 24)
	m.Update(common.ErrorMsg{Err: errors.New("bad \x1b]0;title\x07ref \u202edetails")})
	v := m.View().Content
	if strings.Contains(v, "\x1b]0;") || strings.ContainsRune(v, '\u202e') {
		t.Fatalf("the error screen shows the raw error: %q", v)
	}
	if !strings.Contains(ansi.Strip(v), "bad ref details") {
		t.Fatalf("the error screen lacks the error:\n%s", ansi.Strip(v))
	}
}

// soft browse copies to the clipboard: copying needs no server.
func TestBrowseCopySetsClipboard(t *testing.T) {
	t.Parallel()
	m, err := newModel(t.Context(), newDriveRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	m.SetSize(80, 24)
	_, cmd := m.Update(repo.CopyMsg{Text: "abc1234", Message: "Commit hash copied"})
	want := tea.SetClipboard("abc1234")()
	for _, msg := range msgs(t, cmd) {
		if msg == want {
			return
		}
	}
	t.Fatal("copying did not set the clipboard")
}

// newDriveRepo creates a repository with a README, a file, a directory, two
// commits, a tag and a stash, so that every tab of soft browse has content.
func newDriveRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "drive")
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
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
	gitCmd("init", "-q", "-b", "main", dir)
	write("README.md", "# Drive\n\nreadme-body\n")
	write("file.txt", "one\n")
	write("sub/inner.txt", "inner\n")
	gitCmd("-C", dir, "add", "-A")
	gitCmd("-C", dir, "commit", "-q", "-m", "first-subject")
	write("file.txt", "two\n")
	gitCmd("-C", dir, "commit", "-q", "-am", "second-subject")
	gitCmd("-C", dir, "tag", "v1")
	write("file.txt", "stashed\n")
	gitCmd("-C", dir, "stash", "push", "-q", "-m", "stash-subject")
	r, err := git.OpenLocal(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Soft browse runs the real repository page over a local repository. Its
// repository implements only the proto.Repository methods the page calls and
// leaves the rest to a nil interface, so driving every tab and rendering
// after every message checks that no tab calls one of the others.
func TestBrowseDrivesEveryTab(t *testing.T) {
	t.Parallel()
	m, err := newModel(t.Context(), newDriveRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.common.Zone.Close)

	// send delivers msg and every message the commands produce, rendering
	// the view after each one. Spinner ticks are dropped: they repeat for as
	// long as something loads.
	send := func(msg tea.Msg) {
		t.Helper()
		queue := []tea.Msg{msg}
		for steps := 0; len(queue) > 0; steps++ {
			if steps > 10000 {
				t.Fatal("model never settles")
			}
			msg, queue = queue[0], queue[1:]
			if _, ok := msg.(spinner.TickMsg); ok || msg == nil {
				continue
			}
			_, cmd := m.Update(msg)
			_ = m.View()
			if m.state == errorState {
				t.Fatalf("error after %T: %v", msg, m.error)
			}
			queue = append(queue, msgs(t, cmd)...)
		}
	}
	view := func() string { return ansi.Strip(m.View().Content) }

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, msg := range msgs(t, m.Init()) {
		send(msg)
	}
	if v := view(); !strings.Contains(v, "readme-body") {
		t.Fatalf("readme not shown:\n%s", v)
	}

	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	b := tea.KeyPressMsg{Code: 'b', Text: "b"}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	// Readme, Files, Commits, Stash, Branches and Tags, then back to Readme.
	want := []string{"readme-body", "file.txt", "second-subject", "stash-subject", "main", "v1", "readme-body"}
	for i, w := range want {
		if v := view(); !strings.Contains(v, w) {
			t.Fatalf("tab %d lacks %q:\n%s", i, w, v)
		}
		if i == len(want)-1 {
			break
		}
		keys := []tea.Msg{enter, b, b, esc, esc}
		if w == "main" || w == "v1" {
			// Enter on a reference switches to the Files tab; see below.
			keys = []tea.Msg{b, esc}
		}
		for _, k := range keys {
			send(k)
		}
		send(tab)
	}

	// Picking a branch shows its files.
	shiftTab := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	send(shiftTab)
	send(shiftTab)
	if v := view(); !strings.Contains(v, "main") {
		t.Fatalf("branches not shown:\n%s", v)
	}
	send(enter)
	if v := view(); !strings.Contains(v, "file.txt") {
		t.Fatalf("files of the branch not shown:\n%s", v)
	}
}

// Each load opens the repository again under its own context, so a load
// that was cancelled, as when a later one replaces it or the program
// exits, runs no git; one opened under a live context still reads, and
// stops once that context ends.
func TestRepositoryOpenBindsLoadContext(t *testing.T) {
	t.Parallel()
	repo := repository{r: newDriveRepo(t)}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.Open(cancelled); err == nil {
		t.Fatal("Open under a cancelled load context succeeded")
	}

	live, stop := context.WithCancel(t.Context())
	r, err := repo.Open(live)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := r.HEAD(); err != nil {
		t.Fatalf("HEAD under a live load context: %v", err)
	}
	stop()
	if _, err := r.HEAD(); err == nil {
		t.Fatal("HEAD after the load context ended succeeded")
	}
}

// Toggling the help footer resizes the repository page before the page
// handles the toggle, so the Commits tab loads its page once, by the height
// it has with the footer, rather than by the old height and then again.
func TestBrowseFooterToggleLoadsCommitPageOnce(t *testing.T) {
	t.Parallel()
	m, err := newModel(t.Context(), newDriveRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.common.Zone.Close)

	// send delivers msg and every message the commands produce, and
	// returns how many commit pages were delivered.
	send := func(msg tea.Msg) int {
		t.Helper()
		loads := 0
		queue := []tea.Msg{msg}
		for steps := 0; len(queue) > 0; steps++ {
			if steps > 10000 {
				t.Fatal("model never settles")
			}
			msg, queue = queue[0], queue[1:]
			if _, ok := msg.(spinner.TickMsg); ok || msg == nil {
				continue
			}
			if _, ok := msg.(repo.LogItemsMsg); ok {
				loads++
			}
			_, cmd := m.Update(msg)
			queue = append(queue, msgs(t, cmd)...)
		}
		return loads
	}

	send(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, msg := range msgs(t, m.Init()) {
		send(msg)
	}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	send(tab)
	send(tab)

	help := tea.KeyPressMsg{Code: '?', Text: "?"}
	for i, state := range []string{"shown", "hidden"} {
		if n := send(help); n != 1 {
			t.Fatalf("toggle %d, help %s: the commit page was loaded %d times; want once", i+1, state, n)
		}
	}
}
