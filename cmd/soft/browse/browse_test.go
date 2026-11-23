package browse

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
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

// The reference is loaded only after RepoMsg is handled: batched commands run
// concurrently, so a RefMsg sent alongside RepoMsg could reach the tabs before
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
		t.Fatal("Init sends a RefMsg alongside RepoMsg")
	}
	var repoMsg tea.Msg
	for _, msg := range started {
		if _, ok := msg.(repo.RepoMsg); ok {
			repoMsg = msg
		}
	}
	if repoMsg == nil {
		t.Fatalf("Init sends no RepoMsg: %v", started)
	}

	_, cmd := m.Update(repoMsg)
	if got := msgs(t, cmd); !hasRefMsg(got) {
		t.Fatalf("handling RepoMsg does not load the reference: %v", got)
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
