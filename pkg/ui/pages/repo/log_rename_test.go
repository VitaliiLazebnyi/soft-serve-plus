package repo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/ui/components/selector"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// renameRepo makes a repository at dir whose HEAD renames from to to, with
// names staged straight into the index so any name works.
func renameRepo(t *testing.T, dir, from, to string) {
	t.Helper()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "core.protectNTFS=false"}, args...)...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("", "init", "-q", "-b", "main")
	body := strings.Repeat("same line\n", 20)
	put := func(name, content string) {
		blob := run(content, "hash-object", "-w", "--stdin")
		run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	put(from, body)
	run("", "commit", "-q", "-m", "add")
	run("", "update-index", "--force-remove", "--", from)
	put(to, body+"changed\n")
	run("", "commit", "-q", "-m", "rename")
}

// R1: opening a commit that renames a file to or from a name git quotes
// crashed the server from the diff parser's goroutine. The Log page must
// show the diff with the rename.
func TestLogDiffRenameQuotedName(t *testing.T) {
	t.Parallel()
	names := []string{"café.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, "tab\there.txt", `quo"te.txt`, `back\slash.txt`)
	}
	for _, name := range names {
		for _, d := range []struct{ from, to string }{{"plain.txt", name}, {name, "plain.txt"}} {
			t.Run(fmt.Sprintf("%q to %q", d.from, d.to), func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(t.TempDir(), "repo")
				renameRepo(t, dir, d.from, d.to)
				r, err := git.Open(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := r.HEAD()
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				l := NewLog(uitest.NewCommon(t, ctx, nil, nil))
				l.SetSize(120, 60)
				l.Update(OpenedMsg(openRepo{path: dir}))
				l.Update(RefMsg{Ref: ref})
				l.Update(l.countCommitsCmd()())
				l.Update(l.updateCommitsCmd()())

				// The newest commit, the rename, is selected first.
				l.Update(selector.SelectMsg{IdentifiableItem: l.selector.SelectedItem()})
				msg := l.loadDiffCmd()()
				if _, ok := msg.(LogDiffMsg); !ok {
					t.Fatalf("loadDiffCmd() = %#v; want LogDiffMsg", msg)
				}
				updateRendered(l, msg)
				if l.activeView != logViewDiff {
					t.Fatalf("view = %v; want the diff", l.activeView)
				}
				if !strings.Contains(l.currentDiff.Patch, "rename from ") || !strings.Contains(l.currentDiff.Patch, "+changed") {
					t.Errorf("diff does not show the rename:\n%s", l.currentDiff.Patch)
				}
			})
		}
	}
}
