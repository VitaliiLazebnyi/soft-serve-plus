package browse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/x/ansi"
)

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
	r, err := git.OpenLocal(dir)
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
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	m := newModel(ctx, newDriveRepo(t))
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
