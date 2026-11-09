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
	_, cmd := f.Update(RefMsg{Ref: ref})
	items := findMsg[FileItemsMsg](t, runCmd(cmd))
	f.Update(items)
	if len(items.items) != 1 {
		t.Fatalf("got %d files, want 1", len(items.items))
	}

	// Select the file, then clear the model before its command runs.
	_, cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	f.Update(EmptyRepoMsg{})
	content := findMsg[FileContentMsg](t, runCmd(cmd))
	if content.content != "hello\n" {
		t.Fatalf("content = %q, want the file", content.content)
	}
	// The content of a file opened before the repository was emptied is
	// dropped.
	f.Update(content)
	if f.activeView == filesViewContent || f.Path() != "" {
		t.Fatalf("stale content opened %q", f.Path())
	}

	// Content requested before a ref change is dropped too.
	f.Update(RefMsg{Ref: ref})
	_, cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	f.Update(RefMsg{Ref: ref})
	f.Update(findMsg[FileContentMsg](t, runCmd(cmd)))
	if f.activeView == filesViewContent || f.Path() != "" {
		t.Fatalf("stale content opened %q", f.Path())
	}

	// Open the file and toggle the blame view, then clear the model before
	// the blame command runs.
	_, cmd = f.Update(selector.SelectMsg{IdentifiableItem: items.items[0]})
	f.Update(findMsg[FileContentMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "file.txt" {
		t.Fatalf("path = %q, want file.txt", got)
	}
	_, cmd = f.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	f.Update(EmptyRepoMsg{})
	blame := findMsg[FileBlameMsg](t, runCmd(cmd))
	if blame.blame == nil {
		t.Fatal("blame is nil")
	}
	// The blame of a file opened before the repository was emptied is
	// dropped.
	f.Update(blame)
	if f.activeView == filesViewContent || f.currentBlame != nil {
		t.Fatal("a stale blame opened the content view")
	}
}

func TestLogCommandsDoNotReadModel(t *testing.T) {
	t.Parallel()
	repo, ref := cmdTestRepo(t)
	l := NewLog(cmdTestCommon())
	l.Update(RepoMsg(repo))

	// A new ref, then an empty repository, before the count command runs.
	_, cmd := l.Update(RefMsg{Ref: ref})
	l.Update(EmptyRepoMsg{})
	count := findMsg[LogCountMsg](t, runCmd(cmd))
	if count.count != 1 {
		t.Fatalf("count = %d, want 1", count.count)
	}

	l.Update(RefMsg{Ref: ref})
	_, cmd = l.Update(count)
	l.Update(EmptyRepoMsg{})
	commits := findMsg[LogItemsMsg](t, runCmd(cmd))
	if len(commits.items) != 1 || commits.items[0] == nil {
		t.Fatalf("commits = %#v, want one commit", commits)
	}

	// Open a commit, then switch the ref before the diff command runs.
	commit := commits.items[0].(LogItem).Commit
	l.Update(RefMsg{Ref: ref})
	_, cmd = l.Update(LogCommitMsg(commit))
	l.Update(RefMsg{Ref: ref})
	diff := findMsg[LogDiffMsg](t, runCmd(cmd))

	// The diff of a commit that is no longer selected is dropped.
	l.Update(diff)
	if l.activeView == logViewDiff {
		t.Fatal("a stale diff opened the diff view")
	}

	// Open another commit before the diff of the first one arrives: only the
	// diff of the selected commit is shown.
	// Loading the commits again gives another *git.Commit.
	_, cmd = l.Update(count)
	other := findMsg[LogItemsMsg](t, runCmd(cmd)).items[0].(LogItem).Commit
	if other == commit {
		t.Fatal("reloading the commits returned the same commit")
	}
	_, cmd = l.Update(LogCommitMsg(commit))
	_, otherCmd := l.Update(LogCommitMsg(other))
	l.Update(findMsg[LogDiffMsg](t, runCmd(cmd)))
	if l.activeView == logViewDiff {
		t.Fatal("the diff of a commit that is no longer selected was shown")
	}
	l.Update(findMsg[LogDiffMsg](t, runCmd(otherCmd)))
	if l.activeView != logViewDiff || l.currentDiff == nil {
		t.Fatal("the diff of the selected commit was not shown")
	}
}

// Repo hands a FileContentMsg to the Files pane twice, once as the pane the
// message belongs to and once as the active tab. Opening a file must still
// push its position only once, so that going back returns the cursor to the
// directory the user opened.
func TestFilesBackAfterOpeningFileThroughRepo(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	for _, name := range []string{"a/f", "b/f", "sub/x", "sub/y", "sub/z"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	gr, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	f := NewFiles(cmdTestCommon())
	r := New(cmdTestCommon(), f)
	update := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		_, cmd := r.Update(msg)
		return cmd
	}
	// Give the pane its repository and ref directly: Repo itself only needs
	// them for the status bar.
	f.Update(RepoMsg(openRepo{path: dir}))
	_, cmd := f.Update(RefMsg{Ref: ref})
	update(findMsg[FileItemsMsg](t, runCmd(cmd)))

	// Open "sub", the third directory of the root.
	items := f.selector.Items()
	if len(items) != 3 {
		t.Fatalf("got %d root entries, want 3", len(items))
	}
	f.selector.Select(2)
	cmd = update(selector.SelectMsg{IdentifiableItem: items[2].(selector.IdentifiableItem)})
	update(findMsg[FileItemsMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "sub" {
		t.Fatalf("path = %q, want sub", got)
	}

	// Open "y", the second file of "sub", through Repo.
	items = f.selector.Items()
	f.selector.Select(1)
	cmd = update(selector.SelectMsg{IdentifiableItem: items[1].(selector.IdentifiableItem)})
	update(findMsg[FileContentMsg](t, runCmd(cmd)))
	if got := f.Path(); got != "sub/y" {
		t.Fatalf("path = %q, want sub/y", got)
	}

	// Going back returns to "y" in "sub", then to "sub" in the root.
	for _, want := range []struct {
		path  string
		index int
	}{{"sub", 1}, {"", 2}} {
		cmd = update(GoBackMsg{})
		update(findMsg[FileItemsMsg](t, runCmd(cmd)))
		if got := f.Path(); got != want.path {
			t.Fatalf("path = %q, want %q", got, want.path)
		}
		if got := f.selector.Index(); got != want.index {
			t.Fatalf("in %q the cursor is on entry %d, want %d", want.path, got, want.index)
		}
	}
}
