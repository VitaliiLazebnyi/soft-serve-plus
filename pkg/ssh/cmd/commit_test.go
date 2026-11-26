package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sgit "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
)

// diffRepo creates the repository "diffs" and returns an admin context and
// a function that runs git in a work tree whose pushes go to it.
func diffRepo(t *testing.T) (context.Context, func(stdin string, args ...string) string) {
	t.Helper()
	ctx, be := newAuthTestContext(t)
	ctx = withUser(t, ctx, be, "boss", true)
	_, err := be.CreateRepository(ctx, "diffs", proto.UserFromContext(ctx), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "diffs.git")
	work := t.TempDir()
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "core.protectNTFS=false", "-c", "core.autocrlf=false",
		}, args...)...)
		cmd.Dir = work
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "-b", "main")
	git("", "remote", "add", "origin", bare)
	return ctx, git
}

// `repo commit` on a commit that renames a file to or from a name git
// quotes crashed the server. It must print the commit with the rename.
func TestRepoCommitRenameQuotedName(t *testing.T) {
	names := []string{"café.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\there.txt", `quo"te.txt`, `back\slash.txt`)
	}
	body := strings.Repeat("same line\n", 20)
	for _, name := range names {
		for _, d := range []struct{ from, to string }{{"plain.txt", name}, {name, "plain.txt"}} {
			t.Run(fmt.Sprintf("%q to %q", d.from, d.to), func(t *testing.T) {
				ctx, git := diffRepo(t)
				put := func(path, content string) {
					blob := git(content, "hash-object", "-w", "--stdin")
					git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
				}
				put(d.from, body)
				git("", "commit", "-q", "-m", "add")
				git("", "update-index", "--force-remove", "--", d.from)
				put(d.to, body+"edited-line\n")
				git("", "commit", "-q", "-m", "rename")
				git("", "push", "-q", "origin", "main")
				sha := git("", "rev-parse", "HEAD")

				for _, args := range [][]string{
					{"commit", "diffs", sha},
					{"commit", "--patch", "diffs", sha},
					{"commit", "--color", "diffs", sha},
				} {
					stdout, stderr, err := runRepoOutput(t, ctx, args...)
					if err != nil {
						t.Fatalf("%v: %v: %s", args, err, stderr)
					}
					// The added line's text is in the patch only, not in
					// the stat, as a word like "changed" would be.
					for _, want := range []string{"rename from ", "rename to ", "+edited-line"} {
						if !strings.Contains(stdout, want) {
							t.Errorf("%v: output lacks %q:\n%s", args, want, stdout)
						}
					}
				}
			})
		}
	}
}

// `repo commit --patch | git apply` gives back the commit, including a
// file without a trailing newline, which a patch rebuilt from the parsed
// diff would lose.
func TestRepoCommitPatchApplies(t *testing.T) {
	t.Parallel()
	ctx, git := diffRepo(t)
	write := func(name, content string) {
		blob := git(content, "hash-object", "-w", "--stdin")
		git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	write("a.txt", "one\ntwo")
	write("b.txt", "keep\n")
	git("", "commit", "-q", "-m", "root")
	root := git("", "rev-parse", "HEAD")
	write("a.txt", "one\nTWO")
	write("b.txt", "keep\nno newline")
	git("", "commit", "-q", "-m", "change")
	head := git("", "rev-parse", "HEAD")
	git("", "push", "-q", "origin", "main")

	patch, stderr, err := runRepoOutput(t, ctx, "commit", "--patch", "diffs", head)
	if err != nil {
		t.Fatalf("commit --patch: %v: %s", err, stderr)
	}
	if !strings.Contains(patch, "\\ No newline at end of file\n") {
		t.Errorf("patch lost the no-newline marker:\n%s", patch)
	}
	git("", "read-tree", root)
	git(patch, "apply", "--cached", "-")
	if diff := git("", "diff", "--cached", head); diff != "" {
		t.Errorf("applied patch differs from the commit:\n%s", diff)
	}
}

// A plain --patch over the size cap prints nothing and fails: in a pipeline
// into git apply, a cut patch would apply the files before the cut and
// succeed. The view for reading is cut and says so, and does not fail.
func TestRepoCommitPatchTruncated(t *testing.T) {
	t.Parallel()
	ctx, git := diffRepo(t)
	// A blob just over the patch cap, of short lines that compress well.
	line := "a line of text\n"
	big := strings.Repeat(line, sgit.DiffMaxBytes/len(line)+1)
	blob := git(big, "hash-object", "-w", "--stdin")
	git("", "update-index", "--add", "--cacheinfo", "100644,"+blob+",big.txt")
	git("", "commit", "-q", "-m", "big")
	git("", "push", "-q", "origin", "main")

	stdout, _, err := runRepoOutput(t, ctx, "commit", "--patch", "diffs", "main")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("commit --patch over the cap: err = %v, want a truncation error", err)
	}
	if stdout != "" {
		t.Errorf("commit --patch over the cap printed %q, want nothing", stdout)
	}
	stdout, _, err = runRepoOutput(t, ctx, "commit", "diffs", "main")
	if err != nil || !strings.Contains(stdout, sgit.DiffTruncated) {
		t.Errorf("commit over the cap: err = %v, marker shown = %v", err, strings.Contains(stdout, sgit.DiffTruncated))
	}
}

// git's stat for a binary file ("Bin 2 -> 3 bytes") has no +/- graph; its
// "->" must not be coloured as a deletion. A text file's graph still is.
func TestRenderStatsBinary(t *testing.T) {
	t.Parallel()
	st := styles.DefaultStyles()
	del := st.Log.CommitStatsDel.Render("-")
	if del == "-" {
		t.Skip("the renderer adds no styling")
	}
	out := renderStats(&sgit.Diff{Stat: " b.bin | Bin 2 -> 3 bytes\n t.txt | 2 +-\n"}, st, true)
	lines := strings.Split(out, "\n")
	if strings.Contains(lines[0], del) || !strings.Contains(lines[0], "Bin 2 -> 3 bytes") {
		t.Errorf("binary stat line = %q, want it unstyled", lines[0])
	}
	if !strings.Contains(lines[1], del) {
		t.Errorf("text stat line = %q, want its - styled", lines[1])
	}
}
