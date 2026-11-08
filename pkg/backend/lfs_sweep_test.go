package backend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepIncompleteLFS(t *testing.T) {
	t.Parallel()

	dataPath := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * IncompleteLFSMaxAge)

	write := func(name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(dataPath, "lfs", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}

	stale1 := write("1/incomplete/stale", old)
	stale2 := write("2/incomplete/stale", old)
	fresh := write("1/incomplete/fresh", now)
	object := write("1/objects/ab/cd/abcd", old)
	nested := write("1/incomplete/dir/nested", old)
	// A stray file directly under lfs/ is not a repository.
	stray := write("stray", old)
	// A repository without an incomplete/ directory is skipped.
	if err := os.MkdirAll(filepath.Join(dataPath, "lfs", "3", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := SweepIncompleteLFS(t.Context(), dataPath, now.Add(-IncompleteLFSMaxAge))
	if err != nil {
		t.Fatalf("SweepIncompleteLFS: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d files, want 2", n)
	}

	for _, p := range []string{stale1, stale2} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: want removed, got %v", p, err)
		}
	}
	for _, p := range []string{fresh, object, nested, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: want kept, got %v", p, err)
		}
	}
}

func TestSweepIncompleteLFSNoStorage(t *testing.T) {
	t.Parallel()

	n, err := SweepIncompleteLFS(t.Context(), t.TempDir(), time.Now())
	if n != 0 || err != nil {
		t.Errorf("SweepIncompleteLFS = %d, %v; want 0, nil", n, err)
	}
}
