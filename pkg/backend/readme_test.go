package backend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// A README found only under docs/ is still found. Regression: the glob was
// built with filepath.Join, so on Windows it held a backslash and never
// matched a tree path.
func TestReadmeInSubdirectory(t *testing.T) {
	lt := newLFSTestWith(t, 0, func(work string, _ []lfs.Pointer) {
		if err := os.MkdirAll(filepath.Join(work, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "docs", "README.md"), []byte("# Docs\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	readme, path, err := Readme(lt.repo, nil)
	if err != nil {
		t.Fatalf("Readme: %v", err)
	}
	if path != "docs/README.md" || readme != "# Docs\n" {
		t.Fatalf("Readme = %q, %q; want %q, %q", readme, path, "# Docs\n", "docs/README.md")
	}
}
