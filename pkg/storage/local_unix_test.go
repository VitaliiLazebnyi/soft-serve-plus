//go:build unix

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// LFS objects can hold private repository content, so objects and the
// directories created for them are only accessible to the server's user.
func TestLocalStoragePermissions(t *testing.T) {
	// Keep the umask from masking a mode that is too wide. The umask is per
	// process, so this test must not run in parallel.
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	root := filepath.Join(t.TempDir(), "lfs", "1")
	l := NewLocalStorage(root)
	if _, err := l.Put("incomplete/obj", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if err := l.Rename("incomplete/obj", "objects/ab/cd/obj"); err != nil {
		t.Fatal(err)
	}

	want := map[string]os.FileMode{
		root:                                              0o700,
		filepath.Join(root, "incomplete"):                 0o700,
		filepath.Join(root, "objects"):                    0o700,
		filepath.Join(root, "objects", "ab"):              0o700,
		filepath.Join(root, "objects", "ab", "cd"):        0o700,
		filepath.Join(root, "objects", "ab", "cd", "obj"): 0o600,
	}
	for p, mode := range want {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%s: mode %v, want %v", p, got, mode)
		}
	}
}
