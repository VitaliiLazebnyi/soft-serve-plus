package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// CountGit puts a wrapper named git first on PATH that logs the arguments
// of every git process started from then on, and returns a function that
// returns the logged invocations, one string of space-separated arguments
// each, and clears the log. The wrapper is a shell script, so the test is
// skipped on Windows. It uses t.Setenv, so the test must not be parallel.
func CountGit(t *testing.T) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the counting git wrapper is a shell script")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("finding git: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shQuote(log) + "\nexec " + shQuote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // a test git wrapper must be executable
		t.Fatalf("writing the git wrapper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		t.Helper()
		b, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("reading the git log: %v", err)
		}
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clearing the git log: %v", err)
		}
		var calls []string
		for line := range strings.Lines(string(b)) {
			calls = append(calls, strings.TrimSuffix(line, "\n"))
		}
		return calls
	}
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
