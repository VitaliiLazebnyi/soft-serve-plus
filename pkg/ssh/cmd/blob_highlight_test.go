package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/soft-serve/pkg/ui/uitest"
)

// pushFiles pushes to repository "hooks" a commit that adds files.
func pushFiles(t *testing.T, ctx context.Context, files map[string]string) {
	t.Helper()
	bare := filepath.Join(cmdsOf(ctx).cfg.DataPath, "repos", "hooks.git")
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=a", "-c", "user.email=a@b"}, args...)...) //nolint:noctx
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "files")
	git("push", "-q", bare, "main")
	// The bare repository's HEAD names the host's init.defaultBranch,
	// which may not be main.
	git("--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
}

// blob --color highlights within the same bounds as the TUI: a file whose
// lexer would take seconds, or panics, is printed as plain text.
//
// The command is not timed, has no budget and finds a free slot, so plain
// text is what the bounds leave: the Svelte file is refused by the cap on
// lexers the budget cannot stop; without the cap it comes back highlighted
// many seconds later, minutes under the race detector. The YANG file is
// plain because its lexer's panic is caught.
func TestBlobColorBounded(t *testing.T) {
	t.Setenv("SOFT_SERVE_NO_COLOR", "")
	ctx, be := newAuthTestContext(t)
	cmds := cmdsOf(ctx)
	if cmds.blobBudget != nil {
		t.Fatal("repo blob does not have the default budget")
	}
	asked := false
	cmds.blobBudget = func() <-chan time.Time { asked = true; return nil }
	ownerCtx := whRepo(t, ctx, be)
	pushFiles(t, ownerCtx, map[string]string{
		// Svelte tokenises all of a file up front; 16 KiB of these took
		// 4 s, 256 KiB minutes.
		"app.svelte": strings.Repeat("{a}", (64<<10)/3),
		// The YANG lexer panics on lines of "/*".
		"a.yang":  strings.Repeat("/*\n", 100),
		"main.go": "package main\n\nfunc main() {}\n",
	})

	for _, name := range []string{"app.svelte", "a.yang"} {
		uitest.WaitIdle(t, common.Tokenising)
		stdout, _, err := runRepoOutput(t, ownerCtx, "blob", "--color", "hooks", name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(stdout, "\x1b[") {
			t.Fatalf("%s: printed highlighted; want plain text", name)
		}
	}
	uitest.WaitIdle(t, common.Tokenising)
	stdout, _, err := runRepoOutput(t, ownerCtx, "blob", "--color", "hooks", "main.go")
	if err != nil || !asked || !strings.Contains(stdout, "\x1b[") {
		t.Fatalf("main.go: blob --color = %v, test budget used %v; want it highlighted", err, asked)
	}
}
