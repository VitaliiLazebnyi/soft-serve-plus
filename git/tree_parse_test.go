package git

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestParseLsTreeRecord(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		rec  string
		ok   bool
		kind string
		size int64
		name string
	}{
		{"100644 blob " + id + "      12\tfile", true, "blob", 12, "file"},
		{"100664 blob " + id + "       0\told mode", true, "blob", 0, "old mode"},
		{"100755 blob " + id + " 1234567890\tx", true, "exec", 1234567890, "x"},
		{"120000 blob " + id + "       3\tl", true, "symlink", 3, "l"},
		{"040000 tree " + id + "       -\td", true, "tree", 0, "d"},
		{"160000 commit " + id + "       -\tm", true, "commit", 0, "m"},
		{"100644 blob " + id + "       1\ttab\tand\nnewline", true, "blob", 1, "tab\tand\nnewline"},
		{"100644 blob " + id + "       1\t", false, "", 0, ""},
		{"100644 blob " + id + "       1", false, "", 0, ""},
		{"100644 blob " + strings.ToUpper(id) + "       1\tf", false, "", 0, ""},
		{"100644 blob " + id[:39] + "       1\tf", false, "", 0, ""},
		{"100644 blob " + id + "0       1\tf", false, "", 0, ""},
		{"100644 blob g" + id[1:] + "       1\tf", false, "", 0, ""},
		{"100600 blob " + id + "       1\tf", false, "", 0, ""},
		{"100644 tree " + id + "       1\tf", false, "", 0, ""},
		{"040000 tree " + id + "       1\td", false, "", 0, ""},
		{"100644 blob " + id + "       -\tf", false, "", 0, ""},
		{"100644 blob " + id + "      -1\tf", false, "", 0, ""},
		{"", false, "", 0, ""},
	} {
		e, err := parseLsTreeRecord([]byte(tc.rec))
		if !tc.ok {
			if !errors.Is(err, ErrMalformedTree) {
				t.Errorf("%q: err = %v, want ErrMalformedTree", tc.rec, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.rec, err)
			continue
		}
		if entryKind(e) != tc.kind || e.Size() != tc.size || e.Name() != tc.name || e.ID().String() != id {
			t.Errorf("%q = %s of %d bytes named %q", tc.rec, entryKind(e), e.Size(), e.Name())
		}
	}
}

// The parser takes records split across writes at any byte, and refuses a
// record longer than maxTreeRecord.
func TestLsTreeParserWrites(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef01234567"
	out := "100644 blob " + id + "       5\ta\x00040000 tree " + id + "       -\tdir\x00"
	for step := 1; step <= len(out); step++ {
		p := &lsTreeParser{dir: "top", limit: 10}
		for i := 0; i < len(out); i += step {
			if _, err := p.Write([]byte(out[i:min(i+step, len(out))])); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		if p.err != nil || len(p.pending) != 0 || len(p.ents) != 2 ||
			p.ents[0].path != "top/a" || p.ents[1].path != "top/dir" || !p.ents[1].IsTree() {
			t.Fatalf("step %d: %d entries, err %v, pending %q", step, len(p.ents), p.err, p.pending)
		}
	}

	long := "100644 blob " + id + "       5\t" + strings.Repeat("n", maxTreeRecord)
	for _, chunks := range [][]string{{long + "\x00"}, {long[:100], long[100:] + "\x00"}, {long}} {
		p := &lsTreeParser{limit: 10}
		var err error
		for _, c := range chunks {
			if _, err = p.Write([]byte(c)); err != nil {
				break
			}
		}
		if !errors.Is(err, ErrMalformedTree) {
			t.Errorf("record of %d bytes in %d writes: err = %v, want ErrMalformedTree", len(long), len(chunks), err)
		}
	}
}

// Names that look like pathspec magic or globs are looked up literally.
func TestTreeEntryLiteralPath(t *testing.T) {
	entries := map[string][2]string{
		"a/b":    {"100644", "b"},
		"dir/xy": {"100644", "xy"},
	}
	want := map[string]string{"a/b": "b"}
	missing := []string{":(icase)A/B", ":(exclude)a", ":(glob)a/*"}
	// Git for Windows refuses colons, "*", "?" and brackets in paths.
	if runtime.GOOS != "windows" {
		for name, content := range map[string]string{
			":(glob)q":  "magic",
			":!exclude": "ex",
			"a/*":       "star",
			"[ab]":      "class",
			"dir/x?":    "q",
		} {
			entries[name] = [2]string{"100644", content}
			want[name] = content
		}
		missing = append(missing, "a/?", "dir/x*", "[a]")
	}
	repo, ref := setupTreeRepo(t, entries)
	tree, err := repo.Tree(ref)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	for p, want := range want {
		te, err := tree.TreeEntry(p)
		if err != nil {
			t.Errorf("TreeEntry(%q): %v", p, err)
			continue
		}
		if got, err := te.Contents(); err != nil || string(got) != want {
			t.Errorf("TreeEntry(%q) contents = %q, %v; want %q", p, got, err, want)
		}
	}
	for _, p := range missing {
		if _, err := tree.TreeEntry(p); !errors.Is(err, ErrRevisionNotExist) {
			t.Errorf("TreeEntry(%q) error = %v, want ErrRevisionNotExist", p, err)
		}
	}
}
