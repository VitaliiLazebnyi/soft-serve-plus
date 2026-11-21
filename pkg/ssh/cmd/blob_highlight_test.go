package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

// pushFiles pushes to repository "hooks" a commit that adds files.
func pushFiles(t *testing.T, ctx context.Context, files map[string]string) {
	t.Helper()
	bare := filepath.Join(config.FromContext(ctx).DataPath, "repos", "hooks.git")
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

// runBlobUnbounded runs `repo blob` with args, as runRepoOutput does, with
// no highlighting budget.
func runBlobUnbounded(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := RepoCommand()
	for _, sub := range c.Commands() {
		if sub.Name() == "blob" {
			c.RemoveCommand(sub)
		}
	}
	c.AddCommand(blobCommand(func() <-chan time.Time { return nil }))
	c.SilenceUsage = true
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	c.SetArgs(append([]string{"blob"}, args...))
	err = c.ExecuteContext(ctx)
	return outBuf.String(), errBuf.String(), err
}

// blob --color highlights within the same bounds as the TUI: a file whose
// lexer would take seconds, or panics, is printed as plain text.
//
// The command is not timed, and has no highlighting budget, so that a
// loaded machine does not print main.go plain. Plain text is what the
// bounds leave: the Svelte file is refused by the cap on lexers the budget
// cannot stop; without the cap it comes back highlighted a minute or more
// later. The YANG file is plain because its lexer's panic is caught.
func TestBlobColorBounded(t *testing.T) {
	t.Setenv("SOFT_SERVE_NO_COLOR", "")
	ctx, be := newAuthTestContext(t)
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
		stdout, _, err := runBlobUnbounded(t, ownerCtx, "--color", "hooks", name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(stdout, "\x1b[") {
			t.Fatalf("%s: printed highlighted; want plain text", name)
		}
	}
	stdout, _, err := runBlobUnbounded(t, ownerCtx, "--color", "hooks", "main.go")
	if err != nil || !strings.Contains(stdout, "\x1b[") {
		t.Fatalf("main.go: blob --color = %v; want it highlighted", err)
	}
}
