package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffStatsAndPatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "", "init", dir)
	// A name long enough to leave no room for the histogram on the line.
	long := strings.Repeat("n", 80)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() {
		t.Helper()
		gitRun(t, dir, "", "add", ".")
		gitRun(t, dir, "", "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "c")
	}
	write(long, "a\n")
	write("short", "hello world\nsame\n")
	commit()
	write(long, "a\nb\nc\nd\n")
	write("short", "hello there\nsame\n")
	commit()

	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, err := repo.CatFileCommit("HEAD")
	if err != nil {
		t.Fatalf("CatFileCommit: %v", err)
	}
	diff, err := repo.Diff(c)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	stats := diff.Stats().String()
	if !strings.Contains(stats, long+" | 3 +++\n") {
		t.Errorf("stats for a long name have no histogram:\n%s", stats)
	}

	patch := diff.Patch()
	for _, want := range []string{"\n-hello world\n", "\n+hello there\n", "\n same\n", "\n+b\n"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
}
