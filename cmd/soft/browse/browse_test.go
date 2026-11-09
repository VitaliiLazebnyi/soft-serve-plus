package browse

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
)

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
	case <-time.After(time.Minute):
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
	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	m := newModel(t.Context(), r)
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
	c := common.NewCommon(context.Background(), 80, 24)
	t.Cleanup(c.Zone.Close)
	m := &model{common: c}

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
	case <-time.After(10 * time.Second):
		t.Fatal("render after quit blocked: the zone manager was closed on quit")
	}
}
