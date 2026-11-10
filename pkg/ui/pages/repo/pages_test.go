package repo

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// The commit view shows the author date, as git log does, not the
// committer date.
func TestRenderCommitShowsAuthorDate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first", "--date", "2001-02-03T04:05:06Z")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CatFileCommit(head.ID)
	if err != nil {
		t.Fatal(err)
	}

	l := NewLog(common.NewCommon(context.Background(), 200, 40))
	out := l.renderCommit(c)
	if want := c.Author.When.Format(time.UnixDate); !strings.Contains(out, want) {
		t.Errorf("commit view %q lacks the author date %q", out, want)
	}
	if got := c.Committer.When.Format(time.UnixDate); strings.Contains(out, got) {
		t.Errorf("commit view %q shows the committer date %q", out, got)
	}
}

// References are sorted by the committer date they show, newest first.
func TestRefItemsSortByCommitterDate(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time {
		return time.Date(2020, 1, d, 0, 0, 0, 0, time.UTC)
	}
	// "old" was authored last but committed first.
	old := RefItem{&git.RefInfo{CommitID: "old", AuthorWhen: day(9), CommitterWhen: day(1)}}
	recent := RefItem{&git.RefInfo{CommitID: "recent", AuthorWhen: day(2), CommitterWhen: day(5)}}
	its := RefItems{old, recent}
	sort.Stable(its)
	if its[0].CommitID != recent.CommitID {
		t.Errorf("references sorted by author date: got committer dates %v, %v",
			its[0].CommitterWhen, its[1].CommitterWhen)
	}
}

// An empty directory shows no position rather than "# 1/0".
func TestFilesStatusBarInfoEmpty(t *testing.T) {
	t.Parallel()
	f := NewFiles(common.NewCommon(context.Background(), 80, 24))
	f.activeView = filesViewFiles
	if got := f.StatusBarInfo(); got != "# 0/0" {
		t.Errorf("StatusBarInfo for an empty directory = %q, want %q", got, "# 0/0")
	}
}

// The stash patch is highlighted once, by renderDiff, and shown as is.
func TestStashPatchHighlightedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	file := filepath.Join(dir, "main.go")
	writeFile(t, file, "package main\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	writeFile(t, file, "package main\n\nfunc main() {}\n")
	gitRun(t, dir, "stash", "-q")

	const width = 120
	s := NewStash(common.NewCommon(context.Background(), width, 200))
	s.SetSize(width, 200)
	s.Update(RepoMsg(openRepo{path: dir}))
	s.Update(s.fetchStashCmd()())
	msg, ok := s.fetchStashPatchCmd()().(StashPatchMsg)
	if !ok || msg.Diff == nil {
		t.Fatalf("fetchStashPatchCmd()() = %v, want a patch", msg)
	}
	s.Update(msg)

	// Highlighting the patch again would wrap each line in another code
	// block, with its own margin and color, so it no longer starts as
	// renderDiff rendered it.
	view := strings.Split(s.View(), "\n")
	for _, line := range strings.Split(renderDiff(msg.Diff, width), "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}
		found := false
		for _, v := range view {
			if strings.HasPrefix(v, line) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("stash view has no line starting with the rendered diff line %q:\n%q", line, view)
		}
	}
}

// writeFile writes content to path and fails the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
