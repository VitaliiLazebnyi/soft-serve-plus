package repo

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

// A file with a non-ASCII name, which git C-quotes in ls-tree output, must be
// listed, shown and blamed under its real name.
func TestFilesNonASCIIName(t *testing.T) {
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

	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.Update(RepoMsg(openRepo{path: dir}))
	f.Update(RefMsg(ref))

	// Walk into the directory, then onto the file, like selecting them.
	selectOnly := func(want string) {
		t.Helper()
		items, ok := f.updateFilesCmd().(FileItemsMsg)
		if !ok || len(items) != 1 {
			t.Fatalf("files at %q = %v, want one item", f.path, items)
		}
		item := items[0].(FileItem)
		if item.Title() != want {
			t.Fatalf("item title = %q, want %q", item.Title(), want)
		}
		f.currentItem = &item
		f.path = path.Join(f.path, item.entry.Name())
	}
	selectOnly(dirName)
	selectOnly(fileName)

	msg := f.selectFileCmd()
	content, ok := msg.(FileContentMsg)
	if !ok {
		t.Fatalf("selectFileCmd = %T %v, want FileContentMsg", msg, msg)
	}
	f.Update(content)
	if want := dirName + "/" + fileName; f.StatusBarValue() != want {
		t.Errorf("status bar = %q, want %q", f.StatusBarValue(), want)
	}

	if msg := f.fetchBlame(); !isBlame(msg) {
		t.Fatalf("fetchBlame = %T %v, want FileBlameMsg", msg, msg)
	}
}

func isBlame(msg any) bool {
	_, ok := msg.(FileBlameMsg)
	return ok
}

// A failed blame must return to the content view rather than leave the
// spinner up, so that the user can still go back.
func TestFilesBlameErrorLeavesLoading(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	f := NewFiles(common.NewCommon(ctx, 80, 40))
	f.activeView = filesViewLoading
	f.blameView = true

	f.Update(common.ErrorMsg(errors.New("blame failed")))

	if f.activeView != filesViewContent {
		t.Errorf("active view = %v, want content view", f.activeView)
	}
	if f.blameView {
		t.Error("blame view still on after a failed blame")
	}
}
