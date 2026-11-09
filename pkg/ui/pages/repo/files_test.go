package repo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// Opening a file larger than max_blob_size is refused without reading it,
// and a file that fits is shown.
func TestSelectFileSizeLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", dir)
	files := map[string]string{
		"small.txt": "hello\n",
		"big.txt":   strings.Repeat("x", 100),
		"bin.dat":   "\x00\x01\x02",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")

	r := openRepo{path: dir}
	gr, err := r.Open()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gr.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gr.TreePath(ref, "")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.MaxBlobSize = 50
	ctx := config.WithContext(t.Context(), cfg)
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.repo = r
	f.ref = ref

	sel := func(name string) any {
		te, err := tree.TreeEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		f.currentItem = &FileItem{entry: te}
		return f.selectFileCmd()
	}

	if msg, ok := sel("small.txt").(FileContentMsg); !ok || msg.content != "hello\n" {
		t.Errorf("small.txt: got %#v, want its content", msg)
	}
	if msg, ok := sel("big.txt").(common.ErrorMsg); !ok || !errors.Is(msg, git.ErrFileTooLarge) {
		t.Errorf("big.txt: got %#v, want ErrFileTooLarge", msg)
	}
	if msg, ok := sel("bin.dat").(common.ErrorMsg); !ok || !errors.Is(msg, errBinaryFile) {
		t.Errorf("bin.dat: got %#v, want errBinaryFile", msg)
	}
}

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
