package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

// diskRepo is a repository on disk with a name.
type diskRepo struct {
	fakeRepo
	name, path string
}

func (r diskRepo) Name() string                   { return r.name }
func (r diskRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

// newDiskRepo creates a repository with a README on its branch.
func newDiskRepo(t *testing.T, name, branch, readme string) diskRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", branch, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README.md")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	return diskRepo{name: name, path: dir}
}

// What was loaded for a repository or ref the user has left since arrives
// late and must not be shown in place of the current one.
func TestRepoDropsLateMessages(t *testing.T) {
	a := newDiskRepo(t, "a", "main", "# Readme of A\n")
	b := newDiskRepo(t, "b", "trunk", "# Readme of B\n")
	gitRun(t, b.path, "checkout", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(b.path, "README.md"), []byte("# Readme of B dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, b.path, "commit", "-q", "-am", "dev")
	gitRun(t, b.path, "checkout", "-q", "trunk")

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	readme := NewReadme(c)
	files := NewFiles(c)
	r := New(c, readme, files, NewRefs(c, git.RefsHeads))
	r.SetSize(80, 40)
	readmeView := func() string { return ansi.Strip(readme.View()) }

	// Open A; its ref and loads start, but the user leaves before they
	// arrive.
	r.Update(RepoMsg(a))
	refA := updateRefCmd(t.Context(), a, r.gen)()
	r.Update(refA)
	lateReadmeA := readme.updateReadmeCmd()
	lateFilesA := files.updateFilesCmd()

	// Open B.
	r.Update(RepoMsg(b))
	refB := updateRefCmd(t.Context(), b, r.gen)()

	r.Update(refA)
	if r.ref != nil {
		t.Fatalf("late ref of A shown in B: %v", r.ref.Name())
	}
	r.Update(lateReadmeA())
	if strings.Contains(readmeView(), "Readme of A") {
		t.Fatalf("late readme of A shown in B:\n%s", readmeView())
	}

	r.Update(refB)
	if r.ref == nil || r.ref.Name().Short() != "trunk" {
		t.Fatalf("ref of B = %v; want trunk", r.ref)
	}
	if files.ref == nil || files.ref.Name().Short() != "trunk" {
		t.Fatalf("files ref of B = %v; want trunk", files.ref)
	}
	r.Update(lateReadmeA())
	r.Update(lateFilesA())
	if strings.Contains(readmeView(), "Readme of A") {
		t.Fatalf("late readme of A shown in B:\n%s", readmeView())
	}
	if files.activeView != filesViewLoading {
		t.Fatalf("late files of A shown in B: view %v", files.activeView)
	}

	r.Update(readme.updateReadmeCmd()())
	if v := readmeView(); !strings.Contains(v, "Readme of B") {
		t.Fatalf("readme of B not shown:\n%s", v)
	}

	// Switch B to another ref; the readme of the previous one arrives
	// late.
	lateReadmeTrunk := readme.updateReadmeCmd()
	rr, err := b.Open()
	if err != nil {
		t.Fatal(err)
	}
	refs, err := rr.References()
	if err != nil {
		t.Fatal(err)
	}
	var dev *git.Reference
	for _, ref := range refs {
		if ref.Name().Short() == "dev" {
			dev = ref
		}
	}
	if dev == nil {
		t.Fatal("no dev branch")
	}
	r.Update(switchRefCmd(dev, r.gen)())
	r.Update(lateReadmeTrunk())
	if v := readmeView(); strings.Contains(v, "Readme of B") {
		t.Fatalf("late readme of trunk shown for dev:\n%s", v)
	}
	r.Update(readme.updateReadmeCmd()())
	if v := readmeView(); !strings.Contains(v, "Readme of B dev") {
		t.Fatalf("readme of dev not shown:\n%s", v)
	}
}
