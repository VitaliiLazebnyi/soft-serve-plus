package repo

import (
	"errors"
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
			msg := f.updateFilesCmd()
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
