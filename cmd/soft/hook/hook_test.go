package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	_ "modernc.org/sqlite" // sqlite driver
)

func TestRunCustomHookPropagatesExitError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Skip("test binary is not executable on this platform")
	}

	t.Setenv("SOFT_SERVE_TEST_HELPER", "1")
	t.Setenv("SOFT_SERVE_TEST_HELPER_EXIT", "1")
	err = runCustomHook(
		context.Background(),
		executable,
		io.Reader(nil),
		io.Discard,
		io.Discard,
		"-test.run=TestCustomHookHelperProcess",
	)
	if err == nil {
		t.Fatal("runCustomHook returned nil for a failing custom hook")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runCustomHook error = %T %v, want exec.ExitError", err, err)
	}
}

func TestRunCustomHookIgnoresMissingOrNonExecutableHook(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-hook")
	if err := runCustomHook(context.Background(), missing, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(missing) error = %v", err)
	}

	if runtime.GOOS == "windows" {
		// Windows has no execute bit: every hook file runs.
		return
	}
	nonExecutable := filepath.Join(t.TempDir(), "non-executable-hook")
	if err := os.WriteFile(nonExecutable, []byte("exit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCustomHook(context.Background(), nonExecutable, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(non-executable) error = %v", err)
	}
}

// On Windows a hook file has no execute bit and is run with sh. Regression:
// global custom hooks never ran there, as the execute bit was required.
func TestCustomHookCommandWindows(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	hook := filepath.Join(t.TempDir(), "post-update")
	if err := os.WriteFile(hook, []byte("echo \"ran $1\"\nexit 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	name, args, ok, err := customHookCommand("windows", hook)
	if err != nil || !ok {
		t.Fatalf("customHookCommand(windows) = ok %v, err %v; want a command", ok, err)
	}
	var out strings.Builder
	err = runCommand(context.Background(), nil, &out, io.Discard, name, append(args, "main")...)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("hook error = %v, want exit status 3", err)
	}
	if got := out.String(); got != "ran main\n" {
		t.Fatalf("hook output = %q, want %q", got, "ran main\n")
	}

	if _, _, ok, err := customHookCommand("windows", filepath.Dir(hook)); ok || err != nil {
		t.Fatalf("customHookCommand(windows, dir) = ok %v, err %v; want no command", ok, err)
	}
	if runtime.GOOS != "windows" {
		if _, _, ok, err := customHookCommand(runtime.GOOS, hook); ok || err != nil {
			t.Fatalf("customHookCommand(%s, non-executable) = ok %v, err %v; want no command", runtime.GOOS, ok, err)
		}
	}
}

func TestCustomHookHelperProcess(t *testing.T) {
	if os.Getenv("SOFT_SERVE_TEST_HELPER") != "1" {
		return
	}
	if os.Getenv("SOFT_SERVE_TEST_HELPER_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

// A failing custom hook fails the hook command.
func TestHookErrorReturned(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil && runtime.GOOS == "windows" {
		t.Skip("custom hook is a shell script and sh is not found")
	}
	// Run twice, each with its own data path. Regression: the hook command
	// tree was package-level, and cobra keeps a subcommand's context from
	// its first run, so a second run used the first run's config and found
	// no custom hook in the first run's (deleted) data path.
	for _, name := range []string{"first", "second"} {
		t.Run(name, testHookErrorReturned)
	}
}

func testHookErrorReturned(t *testing.T) {
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(cfg.DataPath, "hooks")
	if err := os.MkdirAll(hooksPath, 0o755); err != nil {
		t.Fatal(err)
	}
	//nolint: gosec
	if err := os.WriteFile(filepath.Join(hooksPath, "post-update"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := &Env{Config: cfg, Logger: log.New(io.Discard)}

	command := NewCommand(env)
	command.SetArgs([]string{"post-update"})
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("hook returned nil for a failing custom hook")
	}
}

// No hook opens the database or leaves one in the data path: the server does
// the work of post-receive and post-update (R53, R55), and pre-receive and
// update only run the custom hook. post-receive, as run by hooks generated
// before soft stopped being called, writes its refs to $SOFT_SERVE_PUSH_REFS
// for the server, and gives them to the custom hook too.
func TestHooksOpenNoDatabase(t *testing.T) {
	zero, one := strings.Repeat("0", 40), strings.Repeat("1", 40)
	refLines := zero + " " + one + " refs/heads/main\n" + zero + " " + one + " refs/tags/v1\n"
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{"pre-receive", nil, refLines},
		{"update", []string{"refs/heads/main", zero, one}, ""},
		{"post-receive", nil, refLines},
		{"post-update", []string{"refs/heads/main", "refs/tags/v1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			refs := filepath.Join(t.TempDir(), "refs")
			if err := os.WriteFile(refs, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(hooks.PushRefsEnv, refs)
			cfg := config.DefaultConfig()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			env := &Env{Config: cfg, Logger: log.New(io.Discard)}

			command := NewCommand(env)
			command.SetArgs(append([]string{tc.name}, tc.args...))
			command.SetIn(strings.NewReader(tc.input))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			// A hook's Env holds no database; nothing it runs opens one
			// in the data path either.
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("hook: %v", err)
			}
			entries, err := os.ReadDir(cfg.DataPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("hook created %s in the data path", e.Name())
			}

			got, err := os.ReadFile(refs)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.name == "post-receive" {
				want = refLines
			}
			if string(got) != want {
				t.Errorf("refs file %q, want %q", got, want)
			}
		})
	}
}

// A pre-receive or post-receive input line too long to read fails the hook,
// before it acts or runs the custom hook on the refs read so far: reading
// stopped at that line, and the refs after it would be left out without a
// word.
func TestHookFailsOnUnreadableInput(t *testing.T) {
	sh := runtime.GOOS != "windows"
	if _, err := exec.LookPath("sh"); err != nil {
		sh = false
	}
	for _, name := range []string{"pre-receive", "post-receive"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := config.DefaultConfig()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "ran")
			if sh {
				hooksPath := filepath.Join(cfg.DataPath, "hooks")
				if err := os.MkdirAll(hooksPath, 0o755); err != nil {
					t.Fatal(err)
				}
				//nolint: gosec
				if err := os.WriteFile(filepath.Join(hooksPath, name), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			env := &Env{Config: cfg, Logger: log.New(io.Discard)}

			zero := strings.Repeat("0", 40)
			one := strings.Repeat("1", 40)
			input := zero + " " + one + " refs/heads/a\n" +
				zero + " " + one + " refs/heads/" + strings.Repeat("b", 70<<10) + "\n" +
				zero + " " + one + " refs/heads/c\n"
			command := NewCommand(env)
			command.SetArgs([]string{name})
			command.SetIn(strings.NewReader(input))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "hook input") {
				t.Fatalf("hook returned %v for input it could not read", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the custom hook ran on part of the input")
			}
		})
	}
}

// The ignored --config flag is marked deprecated, so cobra hides it and says
// so when it is used.
func TestConfigFlagDeprecated(t *testing.T) {
	f := NewCommand(&Env{}).PersistentFlags().Lookup("config")
	if f == nil {
		t.Fatal("no --config flag")
	}
	if f.Deprecated == "" {
		t.Error("--config is not marked deprecated")
	}
}

// A hook run without SOFT_SERVE_DATA_PATH, i.e. not from the server, is
// refused before it opens the backend, which would otherwise create a data
// directory and an empty database in the working directory (the repository).
func TestHookRefusesWithoutDataPath(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	t.Setenv("SOFT_SERVE_DATA_PATH", "")
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	env := &Env{Config: cfg, Logger: log.New(io.Discard)}

	command := NewCommand(env)
	command.SetArgs([]string{"pre-receive"})
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.ExecuteContext(t.Context()); !errors.Is(err, ErrNoDataPath) {
		t.Fatalf("hook returned %v, want %v", err, ErrNoDataPath)
	}

	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("hook created %s in the working directory", e.Name())
	}
}
