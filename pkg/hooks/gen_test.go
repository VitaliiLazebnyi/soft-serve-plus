package hooks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestGenerateHooks(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "test.git")
	_, err := git.Init(repoPath, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	for _, hn := range []string{
		PreReceiveHook,
		UpdateHook,
		PostReceiveHook,
		PostUpdateHook,
	} {
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repoPath, "hooks", hn+".d", "soft-serve")); err != nil {
			t.Fatal(err)
		}
	}
}

// The generated hooks are POSIX sh scripts that work from a repository whose
// path contains spaces and pass stdin, arguments and exit codes through.
func TestGeneratedHooksRunFromPathWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks are run by git's sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	tmp := filepath.Join(t.TempDir(), "data dir")
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "my repo.git")
	if _, err := git.Init(repoPath, true); err != nil {
		t.Fatal(err)
	}
	if err := GenerateHooks(context.TODO(), cfg, "my repo"); err != nil {
		t.Fatal(err)
	}

	// A fake soft binary that records how it was called.
	out := filepath.Join(t.TempDir(), "out")
	bin := filepath.Join(tmp, "soft bin")
	script := "#!/bin/sh\n" +
		"{ printf 'args:'; for a in \"$@\"; do printf ' [%s]' \"$a\"; done; printf '\\n'; cat; } >> \"$OUT\"\n" +
		"exit \"${EXIT:-0}\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	for _, hn := range []string{PreReceiveHook, UpdateHook, PostReceiveHook, PostUpdateHook} {
		for _, p := range []string{
			filepath.Join(repoPath, "hooks", hn),
			filepath.Join(repoPath, "hooks", hn+".d", "soft-serve"),
		} {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(b), "#!/bin/sh\n") {
				t.Errorf("%s does not start with #!/bin/sh", p)
			}
		}
	}

	run := func(hook, stdin, exit string, args ...string) error {
		t.Helper()
		// Run the hook as git does: by path, with GIT_DIR unset.
		cmd := exec.CommandContext(t.Context(), filepath.Join(repoPath, "hooks", hook), args...)
		cmd.Dir = repoPath
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"SOFT_SERVE_BIN_PATH=" + bin,
			"SOFT_SERVE_REPO_NAME=my repo",
			"OUT=" + out,
			"EXIT=" + exit,
		}
		return cmd.Run()
	}

	if err := run(PreReceiveHook, "a b refs/heads/main\n", "0"); err != nil {
		t.Fatalf("pre-receive: %v", err)
	}
	if err := run(UpdateHook, "", "0", "refs/heads/main", "old", "new"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := run(PostUpdateHook, "", "0", "refs/heads/a", "refs/heads/b"); err != nil {
		t.Fatalf("post-update: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "args: [hook] [pre-receive]\na b refs/heads/main\n" +
		"args: [hook] [update] [refs/heads/main] [old] [new]\n\n" +
		"args: [hook] [post-update] [refs/heads/a] [refs/heads/b]\n\n"
	if string(got) != want {
		t.Errorf("hook calls:\n%q\nwant\n%q", got, want)
	}

	// A failing hook fails the git hook with its exit code.
	err = run(PostReceiveHook, "a b refs/heads/main\n", "3")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("post-receive with a failing hook: %v, want exit status 3", err)
	}
}
