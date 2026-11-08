package browse

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/pages/repo"
)

// msgs runs cmd, expanding batches, and returns the messages that arrive
// promptly. Commands that wait (ticks and the like) are left out.
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
	case <-time.After(time.Second):
		return nil
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
	if !hasRefMsg(msgs(t, cmd)) {
		t.Fatal("handling RepoMsg does not load the reference")
	}
}
