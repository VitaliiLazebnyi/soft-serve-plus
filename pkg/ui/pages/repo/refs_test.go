package repo

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
)

// openRepo is a proto.Repository that only opens a repository on disk.
type openRepo struct {
	proto.Repository
	path string
}

func (r openRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// A HEAD that names a branch that does not exist, as in a mirror whose
// remote renamed its default branch, falls back to an existing branch
// rather than an error.
func TestDefaultRef(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "trunk", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, dir, "branch", "dev")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "trunk" {
		t.Fatalf("defaultRef with a valid HEAD = %v, %v; want trunk", ref, err)
	}

	gitRun(t, dir, "symbolic-ref", "HEAD", "refs/heads/gone")
	switch msg := UpdateRefCmd(openRepo{path: dir})().(type) {
	case RefMsg:
		if ref := (*git.Reference)(msg); ref == nil || ref.Name().Short() != "dev" {
			t.Fatalf("UpdateRefCmd with a dangling HEAD = %v; want dev", msg)
		}
	default:
		t.Fatalf("UpdateRefCmd with a dangling HEAD = %T %v; want a RefMsg", msg, msg)
	}
	ref, err = defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "dev" {
		t.Fatalf("defaultRef with a dangling HEAD = %v, %v; want dev", ref, err)
	}
	if ref.ID == "" {
		t.Fatal("defaultRef with a dangling HEAD returned a reference without a commit")
	}

	gitRun(t, dir, "branch", "main", "trunk")
	ref, err = defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "main" {
		t.Fatalf("defaultRef with a dangling HEAD and a main branch = %v, %v; want main", ref, err)
	}
}

// The Tags pane lists tags with their commits and tag subjects, and does not
// list them again when the user switches branches.
func TestRefsLoadsOncePerRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, dir, "tag", "-a", "-m", "release notes", "v1")
	gitRun(t, dir, "tag", "v0")

	r := NewRefs(common.NewCommon(t.Context(), 80, 24), git.RefsTags)
	repo := openRepo{path: dir}
	r.Update(RepoMsg(repo))

	ref, ok := UpdateRefCmd(repo)().(RefMsg)
	if !ok {
		t.Fatal("UpdateRefCmd did not return a RefMsg")
	}
	r.Update(ref)
	if !r.isLoading || !r.loaded {
		t.Fatal("the first RefMsg did not load the tags")
	}
	msg, ok := r.updateItemsCmd()().(RefItemsMsg)
	if !ok {
		t.Fatal("updateItemsCmd did not return RefItemsMsg")
	}
	if len(msg.items) != 2 {
		t.Fatalf("updateItemsCmd listed %d tags; want 2", len(msg.items))
	}
	for _, it := range msg.items {
		item := it.(RefItem)
		if item.CommitID != ref.ID {
			t.Errorf("tag %s has commit %q; want %q", item.Short(), item.CommitID, ref.ID)
		}
		if want := map[string]string{"v1": "release notes", "v0": ""}[item.Short()]; item.TagSubject != want {
			t.Errorf("tag %s has subject %q; want %q", item.Short(), item.TagSubject, want)
		}
	}
	r.SetSize(80, 10)
	r.Update(msg)
	if r.isLoading {
		t.Fatal("the tags are still loading after RefItemsMsg")
	}
	if view := r.View(); !strings.Contains(view, "release notes") || !strings.Contains(view, ref.ID[:7]) {
		t.Errorf("the Tags pane %q lacks the tag subject or the commit", view)
	}

	// Switching branches leaves the listed tags alone.
	r.Update(ref)
	if r.isLoading {
		t.Fatal("a branch switch listed the tags again")
	}

	// Opening the repository again lists them again.
	r.Update(RepoMsg(repo))
	r.Update(ref)
	if !r.isLoading {
		t.Fatal("reopening the repository did not list the tags again")
	}
}

// `l` and `→` open the highlighted branch or tag; they must not also turn
// the page, which would open an item on the next page instead.
func TestRefsSelectKeyOpensHighlightedItem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	for i := range 30 {
		gitRun(t, dir, "branch", fmt.Sprintf("b%02d", i))
	}

	for _, k := range []tea.KeyPressMsg{
		{Code: 'l', Text: "l"},
		{Code: tea.KeyRight},
	} {
		t.Run(k.String(), func(t *testing.T) {
			ctx := config.WithContext(context.Background(), config.DefaultConfig())
			r := NewRefs(common.NewCommon(ctx, 80, 10), git.RefsHeads)
			r.SetSize(80, 10)
			r.Update(RepoMsg(openRepo{path: dir}))
			r.Update(r.updateItemsCmd()())
			if r.selector.TotalPages() < 2 {
				t.Fatalf("want several pages, got %d", r.selector.TotalPages())
			}
			want := r.selector.SelectedItem().ID()

			_, cmd := r.Update(k)
			if p := r.selector.Page(); p != 0 {
				t.Errorf("%s turned the page to %d", k, p)
			}
			var got string
			for _, msg := range runCmd(cmd) {
				if sel, ok := msg.(selector.SelectMsg); ok {
					got = sel.ID()
				}
			}
			if got != want {
				t.Errorf("%s selected %q, want %q", k, got, want)
			}
		})
	}
}
