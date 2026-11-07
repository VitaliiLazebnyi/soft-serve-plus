package repo

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// A file listing that fails to load must report an ErrorMsg, so the error is
// shown and the loading spinner stops, rather than a tea.Cmd that nothing
// ever runs.
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
			f.ref = ref
			f.path = tc.path
			msg := f.updateFilesCmd()
			if _, ok := msg.(common.ErrorMsg); !ok {
				t.Fatalf("updateFilesCmd() = %T %v; want common.ErrorMsg", msg, msg)
			}
		})
	}
}
