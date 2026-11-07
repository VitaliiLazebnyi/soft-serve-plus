package repo

import (
	"context"
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
	ctx := config.WithContext(context.Background(), cfg)
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
