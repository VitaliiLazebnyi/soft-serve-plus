package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matryer/is"
)

func TestParseAttr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want []Attribute
	}{
		{
			in:   "org/example/MyClass.java\x00diff\x00java\x00",
			want: []Attribute{{Name: "diff", Value: "java"}},
		},
		{
			in: "org/example/MyClass.java\x00crlf\x00unset\x00" +
				"org/example/MyClass.java\x00diff\x00java\x00" +
				"org/example/MyClass.java\x00myAttr\x00set\x00",
			want: []Attribute{
				{Name: "crlf", Value: "unset"},
				{Name: "diff", Value: "java"},
				{Name: "myAttr", Value: "set"},
			},
		},
		{
			// Paths that git would quote without -z, and separators that
			// would confuse a ": "-based parser.
			in:   "café \"x\": y.bin\x00binary\x00set\x00",
			want: []Attribute{{Name: "binary", Value: "set"}},
		},
		{
			in:   "README\x00caveat\x00unspecified\x00",
			want: []Attribute{{Name: "caveat", Value: "unspecified"}},
		},
		{
			in:   "",
			want: []Attribute{},
		},
		{
			// Truncated output yields only the complete triples.
			in:   "a\x00text\x00set\x00a\x00diff",
			want: []Attribute{{Name: "text", Value: "set"}},
		},
	}

	is := is.New(t)
	for _, c := range cases {
		is.Equal(parseAttributes([]byte(c.in)), c.want)
	}
}

// setupAttrRepo creates a repository whose committed .gitattributes marks
// *.bin as binary, containing a file whose name git quotes in plain output.
// The file is staged straight into the index so the name need not be valid
// on the host file system.
func setupAttrRepo(t *testing.T, name string) (*Repository, *Reference) {
	t.Helper()
	ctx := context.Background()
	repoPath := filepath.Join(t.TempDir(), "attr-repo")

	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoPath
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}

	cmd := exec.CommandContext(ctx, "git", "init", "-q", repoPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}
	attrs := git("*.bin binary\n", "hash-object", "-w", "--stdin")
	blob := git("\x00\x01\x02", "hash-object", "-w", "--stdin")
	git("", "update-index", "--add", "--cacheinfo", "100644,"+attrs+",.gitattributes")
	git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	git("", "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "-m", "init")

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

// TestCheckAttributesQuotedPath checks both the --source path and the
// temporary-index fallback. It is not parallel because it swaps
// attrSourceSupported.
func TestCheckAttributesQuotedPath(t *testing.T) { //nolint: paralleltest
	// Git for Windows rejects '"' and ':' in index paths (core.protectNTFS),
	// so there the name relies on the non-ASCII character, which git quotes in
	// plain output, plus a space.
	name := "dir/café \"quoted\": file.bin"
	if runtime.GOOS == "windows" {
		name = "dir/café quoted file.bin"
	}
	repo, ref := setupAttrRepo(t, name)

	orig := attrSourceSupported
	t.Cleanup(func() { attrSourceSupported = orig })

	for _, tc := range []struct {
		desc   string
		source bool
	}{
		{"source", true},
		{"index", false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.source && !orig() {
				t.Skip("git check-attr --source requires git 2.40")
			}
			attrSourceSupported = func() bool { return tc.source }

			attrs, err := repo.CheckAttributes(ref, name)
			if err != nil {
				t.Fatalf("CheckAttributes: %v", err)
			}
			got := map[string]string{}
			for _, a := range attrs {
				got[a.Name] = a.Value
			}
			// binary is a macro for -diff -merge -text.
			if got["binary"] != "set" || got["text"] != "unset" {
				t.Fatalf("attributes for %q = %v, want binary set and text unset", name, attrs)
			}
		})
	}
}
