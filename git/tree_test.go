package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// newFilesRepo creates a temp git repo with one commit holding files and
// returns the opened repository and its HEAD reference.
func newFilesRepo(t *testing.T, files map[string][]byte) (*Repository, *Reference) {
	t.Helper()
	ctx := context.Background()

	repoPath := filepath.Join(t.TempDir(), "repo")
	for name, data := range files {
		p := filepath.Join(repoPath, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"init"},
		{"-c", "core.autocrlf=false", "add", "."},
		{"-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-m", "init"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	repo, err := Open(repoPath)
	if err != nil {
		t.Fatalf("failed to open repository: %v", err)
	}
	ref, err := repo.HEAD()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}
	return repo, ref
}

func treeFile(t *testing.T, repo *Repository, ref *Reference, name string) *File {
	t.Helper()
	tree, err := repo.TreePath(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	te, err := tree.TreeEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	return te.File()
}

// allocated returns the bytes allocated while running fn.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestFileIsBinary(t *testing.T) {
	const bigSize = 16 << 20
	big := bytes.Repeat([]byte("0123456789abcde\n"), bigSize/16)
	lateNUL := append(bytes.Repeat([]byte("a"), sniffLen), 0)
	bin := append([]byte{0}, bytes.Repeat([]byte("b"), bigSize)...)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"big.txt":   big,
		"late.txt":  lateNUL,
		"bin.dat":   bin,
		"small.txt": []byte("hello\n"),
		"empty.txt": {},
	})

	for name, want := range map[string]bool{
		"big.txt":   false,
		"late.txt":  false, // the NUL is past the sniffed prefix
		"bin.dat":   true,
		"small.txt": false,
		"empty.txt": false,
	} {
		got, err := treeFile(t, repo, ref, name).IsBinary()
		if err != nil {
			t.Fatalf("%s: IsBinary: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: IsBinary = %v, want %v", name, got, want)
		}
	}

	// Sniffing a large file must not buffer the whole blob.
	f := treeFile(t, repo, ref, "big.txt")
	if n := allocated(func() { _, _ = f.IsBinary() }); n > bigSize/4 {
		t.Errorf("IsBinary allocated %d bytes for a %d byte file", n, bigSize)
	}
}

func TestFileContentsLimit(t *testing.T) {
	const bigSize = 16 << 20
	big := bytes.Repeat([]byte("x"), bigSize)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"big.txt":   big,
		"small.txt": []byte("hello\n"),
	})

	small := treeFile(t, repo, ref, "small.txt")
	for _, limit := range []int64{0, -1, 6, 1 << 20} {
		got, err := small.ContentsLimit(limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if string(got) != "hello\n" {
			t.Errorf("limit %d: got %q", limit, got)
		}
	}
	if _, err := small.ContentsLimit(5); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("limit 5 on 6 bytes: err = %v, want ErrFileTooLarge", err)
	}

	f := treeFile(t, repo, ref, "big.txt")
	var err error
	n := allocated(func() { _, err = f.ContentsLimit(1 << 20) })
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if n > bigSize/4 {
		t.Errorf("refusing a %d byte file allocated %d bytes", bigSize, n)
	}
}

func TestLimitWriter(t *testing.T) {
	t.Parallel()
	w := &limitWriter{limit: 4}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("e")); !errors.Is(err, ErrFileTooLarge) || !w.over {
		t.Fatalf("err = %v, over = %v; want ErrFileTooLarge", err, w.over)
	}
	if w.buf.String() != "abcd" {
		t.Errorf("buffered %q", w.buf.String())
	}
}

func TestLatestFileSkipsBinaryAndTooLarge(t *testing.T) {
	repo, ref := newFilesRepo(t, map[string][]byte{
		"README":           {0x89, 'P', 'N', 'G', 0, 0}, // sorts first
		"README.md":        []byte("# hello\n"),
		"docs/README":      bytes.Repeat([]byte("y"), 100),
		"docs/README.md":   []byte("# docs\n"),
		"large/README.md":  bytes.Repeat([]byte("z"), 100),
		"binary/README.md": {0, 1, 2},
	})

	got, fp, err := LatestFile(repo, ref, "README*", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "README.md" || got != "# hello\n" {
		t.Errorf("got %q at %q, want the text README.md", got, fp)
	}

	// docs/README is over the limit; the next match fits.
	got, fp, err = LatestFile(repo, ref, "docs/README*", 50)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "docs/README.md" || got != "# docs\n" {
		t.Errorf("got %q at %q, want docs/README.md", got, fp)
	}

	if _, _, err := LatestFile(repo, ref, "large/README*", 50); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("only match too large: err = %v, want ErrFileTooLarge", err)
	}
	if _, _, err := LatestFile(repo, ref, "binary/README*", 0); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("only match binary: err = %v, want ErrFileNotFound", err)
	}
}

func TestWriteBlob(t *testing.T) {
	data := append(bytes.Repeat([]byte("line\n"), 1<<16), 0, 1, 2)
	repo, ref := newFilesRepo(t, map[string][]byte{"data.bin": data})
	f := treeFile(t, repo, ref, "data.bin")

	var out bytes.Buffer
	if err := repo.WriteBlob(t.Context(), f.ID(), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Errorf("streamed %d bytes, want the %d byte blob", out.Len(), len(data))
	}
}

func TestTreeEntriesWithSizes(t *testing.T) {
	repo, ref := setupTestRepo(t)

	tree, err := repo.TreePath(ref, "")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err := tree.EntriesWithSizes()
	if err != nil {
		t.Fatalf("EntriesWithSizes failed: %v", err)
	}
	if len(ents) != 1 || !ents[0].IsTree() {
		t.Fatalf("expected one tree entry, got %d", len(ents))
	}
	if !ents[0].sized || ents[0].Size() != 0 {
		t.Fatalf("tree entry: sized=%v size=%d, want sized and 0", ents[0].sized, ents[0].Size())
	}

	sub, err := repo.TreePath(ref, "dot_config")
	if err != nil {
		t.Fatalf("TreePath failed: %v", err)
	}
	ents, err = sub.EntriesWithSizes()
	if err != nil {
		t.Fatalf("EntriesWithSizes failed: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "bat" {
		t.Fatalf("expected the bat entry, got %d entries", len(ents))
	}
	want := int64(len("test content"))
	if !ents[0].sized {
		t.Fatal("entry size was not loaded with the listing")
	}
	if got := ents[0].Size(); got != want {
		t.Fatalf("size = %d, want %d", got, want)
	}
}
