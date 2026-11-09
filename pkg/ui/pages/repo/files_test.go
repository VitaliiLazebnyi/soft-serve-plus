package repo

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// A file listing that fails to load must report an ErrorMsg, so the error is
// shown, rather than a tea.Cmd that nothing ever runs. Fed back to the pane,
// that message must also take the pane off its loading spinner.
func TestFilesUpdateFilesCmdReturnsErrorMsg(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	cases := map[string]struct {
		repo openRepo
		path string
	}{
		"open fails":      {openRepo{path: filepath.Join(dir, "missing")}, ""},
		"tree path fails": {openRepo{path: dir}, "no/such/dir"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFiles(common.NewCommon(ctx, 80, 40))
			f.Update(RepoMsg(tc.repo))
			f.Update(RefMsg(ref))
			if f.activeView != filesViewLoading {
				t.Fatalf("after RefMsg activeView = %v; want filesViewLoading", f.activeView)
			}
			f.path = tc.path
			msg := f.updateFilesCmd()()
			if _, ok := msg.(common.ErrorMsg); !ok {
				t.Fatalf("updateFilesCmd() = %T %v; want common.ErrorMsg", msg, msg)
			}
			f.Update(msg)
			if f.activeView == filesViewLoading {
				t.Fatal("pane still loading after the listing error")
			}
		})
	}
}

// A blame that fails to load must return the pane to the file content with
// blame off, not leave it on the loading spinner.
func TestFilesBlameErrorStopsLoading(t *testing.T) {
	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.activeView = filesViewLoading
	f.blameView = true

	f.Update(common.ErrorMsg(errors.New("blame failed")))
	if f.activeView != filesViewContent {
		t.Errorf("activeView = %v; want filesViewContent", f.activeView)
	}
	if f.blameView {
		t.Error("blame still on after the blame error")
	}
}

// The file listing usually loads while another tab is shown. Its error must
// still reach the Files pane so that pane stops loading.
func TestRepoErrorReachesInactiveFilesPane(t *testing.T) {
	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	c := common.NewCommon(ctx, 80, 40)
	files := NewFiles(c)
	r := New(c, NewReadme(c), files)
	files.activeView = filesViewLoading

	r.Update(common.ErrorMsg(errors.New("listing failed")))
	if files.activeView == filesViewLoading {
		t.Error("inactive Files pane still loading after the error")
	}
}

// The Files pane is reused across repositories and refs. When a reload fails,
// the pane must not show, or let the user open, the previous listing.
func TestFilesListingErrorClearsPreviousItems(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
	f.Update(RefMsg(ref))
	f.Update(f.updateFilesCmd()())
	if n := len(f.selector.Items()); n != 1 {
		t.Fatalf("after the first listing items = %d; want 1", n)
	}

	f.Update(RepoMsg(openRepo{path: filepath.Join(dir, "missing")}))
	f.Update(RefMsg(ref))
	msg := f.updateFilesCmd()()
	if _, ok := msg.(common.ErrorMsg); !ok {
		t.Fatalf("updateFilesCmd() = %T %v; want common.ErrorMsg", msg, msg)
	}
	f.Update(msg)
	if f.activeView != filesViewFiles {
		t.Errorf("activeView = %v; want filesViewFiles", f.activeView)
	}
	if n := len(f.selector.Items()); n != 0 {
		t.Errorf("after the failed reload items = %d; want 0", n)
	}
}

// A file with a non-ASCII name, which git C-quotes in ls-tree output, must be
// listed, shown and blamed under its real name.
func TestFilesNonASCIIName(t *testing.T) {
	// Pin core.quotePath=true, git's default, for every git process the test
	// starts, including the ls-tree that git-module runs, so the name is
	// C-quoted even on a machine whose config sets core.quotePath=false.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.quotePath")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")

	const dirName, fileName = "dossier é", "café.txt"
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.HEAD()
	if err != nil {
		t.Fatal(err)
	}

	ctx := config.WithContext(t.Context(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
	f.Update(RefMsg(ref))

	// Walk into the directory, then onto the file, like selecting them.
	selectOnly := func(want string) FileItem {
		t.Helper()
		items, ok := f.updateFilesCmd()().(FileItemsMsg)
		if !ok || len(items) != 1 {
			t.Fatalf("files at %q = %v, want one item", f.path, items)
		}
		item := items[0].(FileItem)
		if item.Title() != want {
			t.Fatalf("item title = %q, want %q", item.Title(), want)
		}
		f.currentItem = &item
		return item
	}
	dirItem := selectOnly(dirName)
	f.path = path.Join(f.path, dirItem.entry.Name())
	fileItem := selectOnly(fileName)

	msg := f.selectFileCmd(fileItem, path.Join(f.path, fileItem.entry.Name()), 0)()
	content, ok := msg.(FileContentMsg)
	if !ok {
		t.Fatalf("selectFileCmd = %T %v, want FileContentMsg", msg, msg)
	}
	f.Update(content)
	if want := dirName + "/" + fileName; f.StatusBarValue() != want {
		t.Errorf("status bar = %q, want %q", f.StatusBarValue(), want)
	}

	if msg := f.fetchBlameCmd()(); !isBlame(msg) {
		t.Fatalf("fetchBlame = %T %v, want FileBlameMsg", msg, msg)
	}
}

func isBlame(msg any) bool {
	_, ok := msg.(FileBlameMsg)
	return ok
}
