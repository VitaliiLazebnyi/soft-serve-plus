package git

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/charmbracelet/soft-serve/git/gittest"
)

// TestLatestFilePatterns pins which file names a LatestFile pattern
// matches, for the README pattern the server uses and a few others. The
// syntax is path.Match's: negated classes ([^x], not gobwas's [!x]),
// alternatives ({a,b}) and ** are not part of it, and no caller uses them.
func TestLatestFilePatterns(t *testing.T) {
	const readme = "[rR][eE][aA][dD][mM][eE]*"
	cases := []struct {
		pattern, name string
		match         bool
	}{
		{readme, "README", true},
		{readme, "readme", true},
		{readme, "ReadMe.md", true},
		{readme, "README.md.bak", true},
		{readme, "readme-first.txt", true},
		{readme, "xREADME", false},
		{readme, "READM", false},
		{readme, "RE ADME", false},
		{readme, ".README", false},
		{"README*", "README-old", true},
		{"README*", "readme", false},
		{"README", "README", true},
		{"README", "README.md", false},
		{"*.md", "notes.md", true},
		{"*.md", "notes.txt", false},
		{"R?ADME", "README", true},
		{"R?ADME", "RAADME", true},
		{"R?ADME", "RADME", false},
		{"[a-z]*", "readme", true},
		{"[a-z]*", "README", false},
	}
	if runtime.GOOS != "windows" {
		// Git for Windows refuses "*" in paths, and reads "\" as a
		// separator.
		cases = append(cases,
			struct {
				pattern, name string
				match         bool
			}{`R\*`, "R*", true},
			struct {
				pattern, name string
				match         bool
			}{`R\*`, "RX", false},
		)
	}
	entries := map[string][2]string{}
	for i, tc := range cases {
		entries[fmt.Sprintf("c%02d/%s", i, tc.name)] = [2]string{"100644", "text"}
	}
	repo, ref := setupTreeRepo(t, entries)

	for i, tc := range cases {
		dir := fmt.Sprintf("c%02d", i)
		_, fp, err := LatestFile(repo, ref, dir+"/"+tc.pattern, 0)
		switch {
		case tc.match && (err != nil || fp != dir+"/"+tc.name):
			t.Errorf("%q against %q: got %q, %v; want a match", tc.pattern, tc.name, fp, err)
		case !tc.match && !errors.Is(err, ErrFileNotFound):
			t.Errorf("%q against %q: got %q, %v; want no match", tc.pattern, tc.name, fp, err)
		}
	}
}

// A malformed pattern is an error, not a panic.
func TestLatestFileBadPattern(t *testing.T) {
	repo, ref := setupTreeRepo(t, map[string][2]string{"README": {"100644", "x"}})
	if _, _, err := LatestFile(repo, ref, "[README", 0); err == nil {
		t.Fatal("LatestFile with a malformed pattern succeeded")
	}
}

// N:git: LatestFile piped a matching file twice, once to sniff it and once
// to read it. It reads it once.
func TestLatestFileReadsOnce(t *testing.T) { //nolint: paralleltest
	repo, ref := setupTreeRepo(t, map[string][2]string{"README.md": {"100644", "# hello\n"}})
	calls := gittest.CountGit(t)
	if _, _, err := LatestFile(repo, ref, "README*", 0); err != nil {
		t.Fatalf("LatestFile: %v", err)
	}
	reads := 0
	for _, c := range calls() {
		if c == "show "+gitRun(t, repo.Path, "", "rev-parse", "HEAD:README.md") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("LatestFile read the file %d times, want once", reads)
	}
}

// A binary match is skipped after its first bytes, with or without a size
// limit, rather than read in full; one over the limit is skipped as binary,
// not reported as too large.
func TestLatestFileSniffsBinary(t *testing.T) {
	const bigSize = 16 << 20
	bin := append([]byte{0}, bytes.Repeat([]byte("b"), bigSize)...)
	repo, ref := newFilesRepo(t, map[string][]byte{
		"a/README":    bin,
		"b/README":    bin,
		"b/readme.md": []byte("text\n"),
	})
	for _, limit := range []int64{0, 1 << 20} {
		var err error
		n := allocated(func() { _, _, err = LatestFile(repo, ref, "a/README*", limit) })
		if !errors.Is(err, ErrFileNotFound) {
			t.Errorf("limit %d: err = %v, want ErrFileNotFound", limit, err)
		}
		if n > 1<<20 {
			t.Errorf("limit %d: skipping a %d MiB binary file allocated %d KiB", limit, bigSize>>20, n>>10)
		}
		content, fp, err := LatestFile(repo, ref, "b/[rR][eE][aA][dD][mM][eE]*", limit)
		if err != nil || fp != "b/readme.md" || content != "text\n" {
			t.Errorf("limit %d: LatestFile = %q, %q, %v; want b/readme.md", limit, content, fp, err)
		}
	}
}

func TestTextWriter(t *testing.T) {
	t.Parallel()
	w := &textWriter{limit: 10}
	if _, err := w.Write([]byte("0123456789")); err != nil || w.over || w.binary {
		t.Fatalf("10 bytes at limit 10: %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrFileTooLarge) || !w.over {
		t.Fatalf("past the limit: %v", err)
	}

	w = &textWriter{}
	if _, err := w.Write(bytes.Repeat([]byte("a"), sniffLen-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{0}); !errors.Is(err, errBinary) || !w.binary {
		t.Fatalf("NUL at byte %d: %v", sniffLen-1, err)
	}
	w = &textWriter{}
	if _, err := w.Write(append(bytes.Repeat([]byte("a"), sniffLen), 0)); err != nil || w.binary {
		t.Fatalf("NUL past the sniffed bytes: %v", err)
	}
	// Exactly sniffLen bytes, then a NUL in a write of its own.
	w = &textWriter{}
	if _, err := w.Write(bytes.Repeat([]byte("a"), sniffLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte{0}); err != nil || w.binary {
		t.Fatalf("NUL in its own write past the sniffed bytes: %v", err)
	}
}
