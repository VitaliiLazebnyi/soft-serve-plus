package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

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

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	return openRepo{path: dir}, ref
}

func cmdTestCommon() common.Common {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	return common.NewCommon(ctx, 80, 40)
}

// bubbletea runs every command in its own goroutine, after Update returned.
// A command must therefore not read the model, which Update may have changed
// in the meantime: an empty repository or a new ref clears the file and ref
// the blame command needs, and reading them crashed the server.
func TestFilesCommandsDoNotReadModel(t *testing.T) {
	t.Parallel()
	repo, ref := cmdTestRepo(t)
	f := NewFiles(cmdTestCommon())
	f.Update(RepoMsg(repo))
	_, cmd := f.Update(RefMsg(ref))
	items := findMsg[FileItemsMsg](t, runCmd(cmd))
	f.Update(items)
	if len(items) != 1 {
		t.Fatalf("got %d files, want 1", len(items))
	}

	// Select the file, then clear the model before its command runs.
	_, cmd = f.Update(selector.SelectMsg{IdentifiableItem: items[0]})
	f.Update(EmptyRepoMsg{})
	content := findMsg[FileContentMsg](t, runCmd(cmd))
	if content.content != "hello\n" {
		t.Fatalf("content = %q, want the file", content.content)
	}

	// Open the file and toggle the blame view, then clear the model before
	// the blame command runs.
	f.Update(RefMsg(ref))
	f.Update(selector.SelectMsg{IdentifiableItem: items[0]})
	f.Update(content)
	if got := f.Path(); got != "file.txt" {
		t.Fatalf("path = %q, want file.txt", got)
	}
	_, cmd = f.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	f.Update(EmptyRepoMsg{})
	if blame := findMsg[FileBlameMsg](t, runCmd(cmd)); blame == nil {
		t.Fatal("blame is nil")
	}
}

func TestLogCommandsDoNotReadModel(t *testing.T) {
	t.Parallel()
	repo, ref := cmdTestRepo(t)
	l := NewLog(cmdTestCommon())
	l.Update(RepoMsg(repo))

	// A new ref, then an empty repository, before the count command runs.
	_, cmd := l.Update(RefMsg(ref))
	l.Update(EmptyRepoMsg{})
	count := findMsg[LogCountMsg](t, runCmd(cmd))
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}

	l.Update(RefMsg(ref))
	_, cmd = l.Update(count)
	l.Update(EmptyRepoMsg{})
	commits := findMsg[LogItemsMsg](t, runCmd(cmd))
	if len(commits) != 1 || commits[0] == nil {
		t.Fatalf("commits = %#v, want one commit", commits)
	}

	// Open a commit, then switch the ref before the diff command runs.
	l.Update(RefMsg(ref))
	_, cmd = l.Update(LogCommitMsg(commits[0].(LogItem).Commit))
	l.Update(RefMsg(ref))
	diff := findMsg[LogDiffMsg](t, runCmd(cmd))

	// The diff of a commit that is no longer selected is dropped.
	l.Update(diff)
	if l.activeView == logViewDiff {
		t.Fatal("a stale diff opened the diff view")
	}
}
