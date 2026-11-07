package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineCapWriter(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100)
	header := "diff --git a/" + long + " b/" + long + "\n"
	in := "+short\n" + "+" + long + "\n" + header + "-" + long + "\nnoeol" + long
	want := "+short\n" + "+" + long[:15] + "\n" + header + "-" + long[:15] + "\nnoeol" + long[:11]

	// Feed the input whole and in every chunk size, so lines and the header
	// prefix are split across writes.
	for chunk := 1; chunk <= len(in); chunk++ {
		var out bytes.Buffer
		w := newLineCapWriter(&out, 16)
		for s := in; len(s) > 0; {
			n := min(chunk, len(s))
			if got, err := w.Write([]byte(s[:n])); err != nil || got != n {
				t.Fatalf("chunk %d: Write = %d, %v", chunk, got, err)
			}
			s = s[n:]
		}
		if out.String() != want {
			t.Fatalf("chunk %d: got\n%q\nwant\n%q", chunk, out.String(), want)
		}
	}
}

// commitFile adds a commit writing data to name in the repository at path.
func commitFile(t *testing.T, repoPath, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoPath, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-c", "core.autocrlf=false", "add", "."},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "add " + name},
	} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

func TestDiffLongLine(t *testing.T) {
	const bigSize = 32 << 20
	repo, _ := newFilesRepo(t, map[string][]byte{"small.txt": []byte("hello\n")})
	commitFile(t, repo.Path, "line.txt", bytes.Repeat([]byte("x"), bigSize))

	ref, err := repo.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CatFileCommit(ref.ID)
	if err != nil {
		t.Fatal(err)
	}

	var diff *Diff
	n := allocated(func() { diff, err = repo.Diff(commit) })
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if n > bigSize/4 {
		t.Errorf("Diff allocated %d bytes for a %d byte line", n, bigSize)
	}
	if len(diff.Files) != 1 || diff.Files[0].Name != "line.txt" {
		t.Fatalf("files = %+v, want line.txt", diff.Files)
	}
	for _, s := range diff.Files[0].Sections {
		for _, l := range s.Lines {
			if len(l.Content) > DiffMaxLineChars {
				t.Errorf("kept a %d byte line", len(l.Content))
			}
		}
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	repo, ref := newFilesRepo(t, map[string][]byte{"a.txt": []byte("one\n")})

	// The root commit diffs against nothing.
	root, err := repo.CatFileCommit(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := repo.Diff(root)
	if err != nil {
		t.Fatalf("Diff(root): %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Name != "a.txt" {
		t.Fatalf("root diff files = %+v", diff.Files)
	}

	commitFile(t, repo.Path, "a.txt", []byte("one\ntwo\n"))
	ref, err = repo.HEAD()
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CatFileCommit(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	diff, err = repo.Diff(c)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := diff.Patch(); !strings.Contains(got, "+two") || strings.Contains(got, "+one") {
		t.Errorf("patch = %q, want only +two added", got)
	}
}
