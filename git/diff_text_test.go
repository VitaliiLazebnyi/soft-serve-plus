package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// diffPatch and diffStat return the two texts callers take from a Diff.
func diffPatch(d *Diff) string { return d.Patch }
func diffStat(d *Diff) string  { return d.Stat }

// diffRepo is a work tree for building commits in diff tests.
type diffRepo struct {
	t   *testing.T
	dir string
}

func newDiffRepo(t *testing.T) *diffRepo {
	t.Helper()
	r := &diffRepo{t: t, dir: filepath.Join(t.TempDir(), "repo")}
	r.git("", "init", "-q", "-b", "main", r.dir)
	return r
}

// git runs git in the work tree with a fixed identity, no line ending
// conversion and no user or system config.
func (r *diffRepo) git(stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{
		"-c", "user.email=test@example.com", "-c", "user.name=Test",
		"-c", "core.autocrlf=false", "-c", "core.protectNTFS=false",
	}, args...)...)
	if _, err := os.Stat(r.dir); err == nil {
		cmd.Dir = r.dir
	}
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// put stages content at path straight into the index, so names that are not
// valid on every file system (tabs, quotes, backslashes) work too.
func (r *diffRepo) put(path, content string) {
	r.t.Helper()
	blob := r.git(content, "hash-object", "-w", "--stdin")
	r.git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
}

// rm removes path from the index.
func (r *diffRepo) rm(path string) {
	r.t.Helper()
	r.git("", "update-index", "--force-remove", "--", path)
}

// commit commits the index and returns the commit.
func (r *diffRepo) commit(msg string) *Commit {
	r.t.Helper()
	r.git("", "commit", "-q", "--allow-empty", "-m", msg)
	return r.head()
}

func (r *diffRepo) open() *Repository {
	r.t.Helper()
	repo, err := Open(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return repo
}

func (r *diffRepo) head() *Commit {
	r.t.Helper()
	c, err := r.open().CatFileCommit("HEAD")
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *diffRepo) diff(c *Commit) *Diff {
	r.t.Helper()
	d, err := r.open().Diff(c)
	if err != nil {
		r.t.Fatalf("Diff: %v", err)
	}
	return d
}

func wantContains(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, s)
		}
	}
}

func wantLacks(t *testing.T, what, s string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(s, n) {
			t.Errorf("%s has %q:\n%s", what, n, s)
		}
	}
}

// A root commit's diff shows every file as new.
func TestDiffPinRootCommit(t *testing.T) {
	r := newDiffRepo(t)
	r.put("a.txt", "one\n")
	d := r.diff(r.commit("root"))
	wantContains(t, "patch", diffPatch(d),
		"diff --git a/a.txt b/a.txt\nnew file mode 100644\n",
		"--- /dev/null\n+++ b/a.txt\n",
		"\n+one\n")
	wantContains(t, "stat", diffStat(d), "a.txt", "1 file changed, 1 insertion(+)")
}

// A merge commit's diff is against its first parent: it shows what the merge
// brought in from the other branch, not the first parent's own changes.
func TestDiffPinMergeFirstParent(t *testing.T) {
	r := newDiffRepo(t)
	r.put("base.txt", "base\n")
	r.commit("base")
	r.git("", "checkout", "-q", "-b", "feature")
	r.put("feature.txt", "from-feature\n")
	r.commit("feature")
	r.git("", "checkout", "-q", "main")
	r.put("main.txt", "from-main\n")
	r.commit("main")
	r.git("", "merge", "-q", "--no-ff", "-m", "merge", "feature")
	d := r.diff(r.head())
	wantContains(t, "patch", diffPatch(d), "diff --git a/feature.txt b/feature.txt", "+from-feature")
	wantLacks(t, "patch", diffPatch(d), "main.txt", "from-main")
}

// A binary file is named in the patch with git's "Binary files" line, not
// its contents.
func TestDiffPinBinary(t *testing.T) {
	r := newDiffRepo(t)
	r.put("data.bin", "bin\x00ary\x01")
	r.commit("one")
	r.put("data.bin", "bin\x00ary\x02")
	d := r.diff(r.commit("two"))
	wantContains(t, "patch", diffPatch(d),
		"diff --git a/data.bin b/data.bin\n",
		"Binary files a/data.bin and b/data.bin differ\n")
	wantLacks(t, "patch", diffPatch(d), "\x00", "ary\x02")
	wantContains(t, "stat", diffStat(d), "data.bin", "1 file changed")
}

// A rename between plain names shows as a rename.
func TestDiffPinPlainRename(t *testing.T) {
	r := newDiffRepo(t)
	body := strings.Repeat("same line\n", 20)
	r.put("old.txt", body)
	r.commit("one")
	r.rm("old.txt")
	r.put("new.txt", body+"one more\n")
	d := r.diff(r.commit("rename"))
	wantContains(t, "patch", diffPatch(d),
		"diff --git a/old.txt b/new.txt\n", "rename from old.txt\n", "rename to new.txt\n", "+one more\n")
}

// A stash's diff is its work tree change against the commit it was made on.
func TestDiffPinStash(t *testing.T) {
	r := newDiffRepo(t)
	r.git("", "commit", "-q", "--allow-empty", "-m", "root")
	if err := os.WriteFile(filepath.Join(r.dir, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.git("", "add", "f.txt")
	r.commit("one")
	if err := os.WriteFile(filepath.Join(r.dir, "f.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.git("", "stash", "-q")
	d, err := r.open().StashDiff(0)
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "patch", diffPatch(d), "diff --git a/f.txt b/f.txt\n", "\n one\n+two\n")
	wantContains(t, "stat", diffStat(d), "f.txt", "1 file changed, 1 insertion(+)")
}

// quotedNames are file names git C-quotes in a diff header by default: a
// non-ASCII character, a tab, a double quote and a backslash. Git refuses the
// last three as paths on Windows.
func quotedNames() []string {
	names := []string{"café.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\there.txt", `quo"te.txt`, `back\slash.txt`)
	}
	return names
}

// R1: renaming a file to or from a name git quotes made the diff parser
// slice with -1 in a goroutine without a recover, which ended the process.
// The diff must come back, showing both names.
func TestDiffRenameQuotedName(t *testing.T) {
	body := strings.Repeat("same line\n", 20)
	for _, name := range quotedNames() {
		for _, dir := range []struct{ from, to string }{{"plain.txt", name}, {name, "plain.txt"}} {
			t.Run(fmt.Sprintf("%q to %q", dir.from, dir.to), func(t *testing.T) {
				r := newDiffRepo(t)
				r.put(dir.from, body)
				r.commit("one")
				r.rm(dir.from)
				r.put(dir.to, body+"changed\n")
				d := r.diff(r.commit("rename"))
				patch := diffPatch(d)
				wantContains(t, "patch", patch, "rename from ", "rename to ", "+changed\n")
				for _, n := range []string{dir.from, dir.to} {
					// Names git still quotes keep their escapes; the plain
					// part is there either way.
					wantContains(t, "patch", patch, strings.SplitN(n, ".", 2)[0][:3])
				}
			})
		}
	}
}

// R43: the patch is git's own text, so it applies and gives back a file
// without a trailing newline.
func TestDiffPatchAppliesNoNewline(t *testing.T) {
	r := newDiffRepo(t)
	r.put("a.txt", "one\ntwo")
	r.put("b.txt", "keep\n")
	root := r.commit("root")
	r.put("a.txt", "one\nTWO")
	r.put("b.txt", "keep\nno newline")
	head := r.commit("change")
	patch := diffPatch(r.diff(head))
	if got := strings.Count(patch, "\\ No newline at end of file\n"); got != 3 {
		t.Errorf("patch has %d no-newline markers, want 3:\n%s", got, patch)
	}

	r.git("", "checkout", "-q", root.ID.String())
	r.git(patch, "apply", "--index", "-")
	if diff := r.git("", "diff", "--cached", head.ID.String()); diff != "" {
		t.Errorf("applied patch differs from the commit:\n%s", diff)
	}
}

// R6 (source half): a commit's diff has a total size cap, not only caps per
// file and per line. 20 files of 1000 lines of 999 bytes (one blob, a few
// KiB in the repository) give a patch of about 19 MiB.
func TestDiffTotalCap(t *testing.T) {
	wantMax := DiffMaxBytes
	r := newDiffRepo(t)
	r.git("", "commit", "-q", "--allow-empty", "-m", "root")
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "%04d%s\n", i, strings.Repeat("x", 994))
	}
	blob := r.git(b.String(), "hash-object", "-w", "--stdin")
	for i := range 20 {
		r.git("", "update-index", "--add", "--cacheinfo", fmt.Sprintf("100644,%s,f%02d.txt", blob, i))
	}
	d := r.diff(r.commit("big"))
	patch := diffPatch(d)
	if len(patch) > wantMax+1024 {
		t.Errorf("patch is %d bytes, want at most about %d", len(patch), wantMax)
	}
	if !strings.HasSuffix(patch, "[diff truncated]\n") {
		t.Errorf("cut patch does not end with the truncation marker: %q", patch[max(0, len(patch)-200):])
	}
	if !d.Truncated {
		t.Error("Truncated = false for a cut diff")
	}
	// The patch is cut at a line, before the marker.
	if body := strings.TrimSuffix(patch, DiffTruncated); !strings.HasSuffix(body, "\n") {
		t.Errorf("patch cut inside a line: %q", body[max(0, len(body)-80):])
	}
}

// setDiffLimits sets the default diff limits for one test.
func setDiffLimits(t *testing.T, l DiffLimits) {
	t.Helper()
	prev := DefaultDiffLimits()
	DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars, DiffMaxBytes = l.Files, l.FileLines, l.LineBytes, l.Bytes
	t.Cleanup(func() {
		DiffMaxFiles, DiffMaxFileLines, DiffMaxLineChars, DiffMaxBytes = prev.Files, prev.FileLines, prev.LineBytes, prev.Bytes
	})
}

// capsRepo returns a commit changing four files: a.txt with 2 lines,
// b.txt with 50 lines, c.txt with one 200-byte line and d.txt with 2
// lines.
func capsRepo(t *testing.T) (*diffRepo, *Commit) {
	t.Helper()
	r := newDiffRepo(t)
	r.git("", "commit", "-q", "--allow-empty", "-m", "root")
	var many strings.Builder
	for i := range 50 {
		fmt.Fprintf(&many, "line %d\n", i)
	}
	r.put("a.txt", "a1\na2\n")
	r.put("b.txt", many.String())
	r.put("c.txt", strings.Repeat("y", 200)+"\n")
	r.put("d.txt", "d1\nd2\n")
	return r, r.commit("four files")
}

// Each cap cuts the diff where it says, marks it truncated, and leaves what
// is under the caps as git printed it.
func TestDiffCaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits DiffLimits
		want   []string
		lacks  []string
		cut    bool
	}{
		{
			name:   "under every cap",
			limits: DiffLimits{Files: 10, FileLines: 100, LineBytes: 1000, Bytes: 1 << 20},
			want:   []string{"+a1\n", "+line 49\n", "+" + strings.Repeat("y", 200) + "\n", "+d2\n"},
		},
		{
			name:   "files",
			limits: DiffLimits{Files: 2},
			want:   []string{"diff --git a/a.txt", "diff --git a/b.txt", "+line 49\n"},
			lacks:  []string{"c.txt b/c.txt", "d.txt b/d.txt"},
			cut:    true,
		},
		{
			name:   "file lines",
			limits: DiffLimits{FileLines: 10},
			want:   []string{"+line 3\n" + diffFileTruncated + "diff --git a/c.txt", "+d2\n"},
			lacks:  []string{"+line 4\n"},
			cut:    true,
		},
		{
			name:   "line bytes",
			limits: DiffLimits{LineBytes: 100},
			want:   []string{"\n+" + strings.Repeat("y", 99) + "\n", "+d2\n"},
			lacks:  []string{strings.Repeat("y", 100)},
			cut:    true,
		},
		{
			name:   "bytes",
			limits: DiffLimits{Bytes: 600},
			want:   []string{"+a1\n"},
			lacks:  []string{"+d2\n"},
			cut:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := capsRepo(t)
			d, err := r.open().DiffLimited(c, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			wantContains(t, "patch", d.Patch, tc.want...)
			wantLacks(t, "patch", d.Patch, tc.lacks...)
			if d.Truncated != tc.cut || strings.HasSuffix(d.Patch, DiffTruncated) != tc.cut {
				t.Errorf("Truncated = %v, marker = %v; want %v:\n%s", d.Truncated, strings.HasSuffix(d.Patch, DiffTruncated), tc.cut, d.Patch)
			}
			if tc.limits.Bytes > 0 && len(d.Stat)+len(d.Patch) > tc.limits.Bytes+len(DiffTruncated) {
				t.Errorf("diff is %d bytes, over the %d byte cap", len(d.Stat)+len(d.Patch), tc.limits.Bytes)
			}
			wantContains(t, "stat", d.Stat, "a.txt", "files changed")
		})
	}
}

// The default limits apply to Diff; PatchLimits caps only the total.
func TestDiffDefaultAndPatchLimits(t *testing.T) {
	setDiffLimits(t, DiffLimits{Files: 1000, FileLines: 10, LineBytes: 1000, Bytes: 1 << 20})
	r, c := capsRepo(t)
	if d := r.diff(c); !d.Truncated || strings.Contains(d.Patch, "+line 49\n") {
		t.Errorf("Diff ignores DiffMaxFileLines:\n%s", d.Patch)
	}
	d, err := r.open().DiffLimited(c, PatchLimits())
	if err != nil {
		t.Fatal(err)
	}
	if d.Truncated || !strings.Contains(d.Patch, "+line 49\n") {
		t.Errorf("PatchLimits cut the diff by lines:\n%s", d.Patch)
	}
}

// The writer gives the same result however git's output is split into
// writes, and keeps no more than the line cap of an unfinished line.
func TestDiffWriterChunks(t *testing.T) {
	in := " a | 2 +-\n 1 file changed\n\n" +
		"diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-" + strings.Repeat("o", 50) + "\n+new\n" +
		"diff --git a/b b/b\n--- a/b\n+++ b/b\n@@ -1,3 +1,3 @@\n+1\n+2\n+3\n+4\n" +
		"diff --git a/c b/c\n+last"
	limits := DiffLimits{FileLines: 6, LineBytes: 20}
	var want *Diff
	for chunk := 1; chunk <= len(in); chunk++ {
		w := &diffWriter{limits: limits, statEnd: -1}
		for s := in; len(s) > 0; {
			n := min(chunk, len(s))
			if got, err := w.Write([]byte(s[:n])); err != nil || got != n {
				t.Fatalf("chunk %d: Write = %d, %v", chunk, got, err)
			}
			if len(w.line) > max(limits.LineBytes, len(diffHeader))+1 {
				t.Fatalf("chunk %d: holds %d bytes of a line", chunk, len(w.line))
			}
			s = s[n:]
		}
		w.flushLine()
		got := w.diff()
		if want == nil {
			want = got
			continue
		}
		if *got != *want {
			t.Fatalf("chunk %d: got %+v, want %+v", chunk, got, want)
		}
	}
	if want.Stat != " a | 2 +-\n 1 file changed\n" {
		t.Errorf("Stat = %q", want.Stat)
	}
	wantPatch := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-" + strings.Repeat("o", 19) + "\n+new\n" +
		"diff --git a/b b/b\n--- a/b\n+++ b/b\n@@ -1,3 +1,3 @@\n+1\n+2\n" + diffFileTruncated +
		"diff --git a/c b/c\n+last\n" + DiffTruncated
	if want.Patch != wantPatch || !want.Truncated {
		t.Errorf("Patch = %q, Truncated = %v\nwant %q", want.Patch, want.Truncated, wantPatch)
	}
}

// A huge line or a huge file costs memory by the caps, not by its size.
func TestDiffHugeMemory(t *testing.T) {
	const size = 32 << 20
	for _, tc := range []struct {
		name, content string
	}{
		{"one line", strings.Repeat("x", size)},
		{"many lines", strings.Repeat("x\n", size/2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDiffRepo(t)
			r.git("", "commit", "-q", "--allow-empty", "-m", "root")
			r.put("big.txt", tc.content)
			r.put("z.txt", "after\n")
			c := r.commit("big")
			repo := r.open()
			var d *Diff
			var err error
			n := allocated(func() { d, err = repo.Diff(c) })
			if err != nil {
				t.Fatal(err)
			}
			if n > size/4 {
				t.Errorf("Diff allocated %d bytes for a %d byte file", n, size)
			}
			if !d.Truncated || !strings.Contains(d.Patch, "+after\n") {
				t.Errorf("Truncated = %v, the file after the big one shown = %v", d.Truncated, strings.Contains(d.Patch, "+after\n"))
			}
		})
	}
}
