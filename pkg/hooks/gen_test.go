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

// recorder is a script that appends how it was called, its arguments and
// stdin, to $OUT, and exits with $EXIT.
const recorder = "#!/bin/sh\n" +
	"{ printf '%s args:' \"$(basename \"$0\")\"; for a in \"$@\"; do printf ' [%s]' \"$a\"; done; printf '\\n'; cat; } >> \"$OUT\"\n" +
	"exit \"${EXIT:-0}\"\n"

// The generated hooks are POSIX sh scripts that work from a repository whose
// path contains spaces and pass stdin, arguments and exit codes through.
// pre-receive and update run the server-wide custom hook themselves and start
// no soft process (R55); post-receive and post-update run soft.
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

	// A fake soft binary and server-wide custom hooks that record how they
	// were called, each to its own file.
	binOut := filepath.Join(t.TempDir(), "bin")
	hookOut := filepath.Join(t.TempDir(), "custom")
	bin := filepath.Join(tmp, "soft bin")
	if err := os.WriteFile(bin, []byte(recorder), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	customDir := filepath.Join(tmp, "hooks")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hn := range []string{PreReceiveHook, UpdateHook} {
		if err := os.WriteFile(filepath.Join(customDir, hn), []byte(recorder), 0o755); err != nil { //nolint:gosec
			t.Fatal(err)
		}
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
		out := binOut
		if hook == PreReceiveHook || hook == UpdateHook {
			out = hookOut
		}
		// Run the hook as git does: by path, with GIT_DIR unset.
		cmd := exec.CommandContext(t.Context(), filepath.Join(repoPath, "hooks", hook), args...)
		cmd.Dir = repoPath
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"SOFT_SERVE_BIN_PATH=" + bin,
			"SOFT_SERVE_DATA_PATH=" + tmp,
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
	for _, c := range []struct{ file, want string }{{
		// The custom hooks get what `soft hook` gave them: the ref lines
		// for pre-receive, the three arguments and no input for update.
		file: hookOut,
		want: "pre-receive args:\na b refs/heads/main\n" +
			"update args: [refs/heads/main] [old] [new]\n",
	}, {
		// soft runs only for post-update here, so pre-receive and update
		// start no soft process.
		file: binOut,
		want: "soft bin args: [hook] [post-update] [refs/heads/a] [refs/heads/b]\n\n",
	}} {
		got, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("calls:\n%q\nwant\n%q", got, c.want)
		}
	}

	// A failing custom hook or soft fails the git hook with its exit code.
	for _, hn := range []string{UpdateHook, PostReceiveHook} {
		err := run(hn, "a b refs/heads/main\n", "3", "refs/heads/main", "old", "new")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Errorf("%s with a failing hook: %v, want exit status 3", hn, err)
		}
	}

	// A custom hook that is not executable does not run.
	if err := os.Chmod(filepath.Join(customDir, UpdateHook), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(UpdateHook, "", "3", "refs/heads/main", "old", "new"); err != nil {
		t.Errorf("update with a non-executable custom hook: %v, want success", err)
	}
}

// On Windows, which has no execute bit, the soft-serve script runs any custom
// hook file with sh, as `soft hook` did there.
func TestSoftServeHookWindows(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not executable: Windows has no execute bit.
	if err := os.WriteFile(filepath.Join(tmp, "hooks", UpdateHook), []byte("echo \"ran $1 $2 $3\"\nexit 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "soft-serve")
	if err := os.WriteFile(script, []byte(softServeHook("windows", UpdateHook)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", script, "refs/heads/main", "old", "new")
	cmd.Env = append(os.Environ(), "SOFT_SERVE_DATA_PATH="+tmp, "SOFT_SERVE_REPO_NAME=repo")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("update: %v, want exit status 3", err)
	}
	if got, want := string(out), "ran refs/heads/main old new\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

// Without SOFT_SERVE_DATA_PATH a custom hook cannot be found, and the hook
// refuses the push, as `soft hook` did, rather than look for one elsewhere.
func TestSoftServeHookNeedsDataPath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	for _, hn := range []string{PreReceiveHook, UpdateHook} {
		script := filepath.Join(t.TempDir(), "soft-serve")
		if err := os.WriteFile(script, []byte(softServeHook(runtime.GOOS, hn)), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", script, "refs/heads/main", "old", "new")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SOFT_SERVE_REPO_NAME=repo"}
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("%s without a data path: %v, want exit status 1", hn, err)
		}
		if !strings.Contains(string(out), "SOFT_SERVE_DATA_PATH is not set") {
			t.Errorf("%s without a data path printed %q", hn, out)
		}
	}
}

func initRepo(t *testing.T) (*config.Config, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	repoPath := filepath.Join(tmp, "repos", "test.git")
	if _, err := git.Init(repoPath, true); err != nil {
		t.Fatal(err)
	}
	return cfg, repoPath
}

func TestGenerateHooksRestoresExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes have no executable bit on Windows")
	}

	cfg, repoPath := initRepo(t)
	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, hn := range []string{PreReceiveHook, UpdateHook, PostReceiveHook, PostUpdateHook} {
		paths = append(paths,
			filepath.Join(repoPath, "hooks", hn),
			filepath.Join(repoPath, "hooks", hn+".d", "soft-serve"),
		)
	}

	// Simulate hooks restored from a backup without the executable bit.
	for _, p := range paths {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err != nil {
		t.Fatal(err)
	}

	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 != 0o111 {
			t.Errorf("%s: mode %v is not executable", p, fi.Mode().Perm())
		}
	}
}

func TestGenerateHooksReturnsWriteError(t *testing.T) {
	cfg, repoPath := initRepo(t)

	// A directory in place of the soft-serve hook makes writing it fail.
	if err := os.MkdirAll(filepath.Join(repoPath, "hooks", UpdateHook+".d", "soft-serve"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := GenerateHooks(context.TODO(), cfg, "test.git"); err == nil {
		t.Fatal("expected an error when a hook can't be written")
	}
}
