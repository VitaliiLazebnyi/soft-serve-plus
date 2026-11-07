package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestContentsPreallocates checks that File.Contents reads a big file into a
// buffer sized for it instead of growing one by doubling.
func TestContentsPreallocates(t *testing.T) {
	repo, _ := setupTestRepo(t)
	const size = 1<<20 + 123
	want := bytes.Repeat([]byte("abcdefghij"), size/10+1)[:size]
	if err := os.WriteFile(filepath.Join(repo.Path, "big"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "big"},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "big"},
	} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = repo.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
	}
	r := repo.WithContext(context.Background())
	head, err := r.HEAD()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	tree, err := r.Tree(head)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	entry, err := tree.TreeEntry("big")
	if err != nil {
		t.Fatalf("TreeEntry: %v", err)
	}
	got, err := entry.Contents()
	if err != nil {
		t.Fatalf("Contents: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Contents returned %d bytes that differ from the %d written", len(got), len(want))
	}
	// Doubling would leave a capacity of at least 1.5 MiB here.
	if c := cap(got); c > size+64*1024 {
		t.Fatalf("Contents buffer capacity = %d for a %d-byte file; want it preallocated", c, size)
	}
}
