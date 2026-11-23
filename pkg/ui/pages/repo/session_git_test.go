//go:build !windows

package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// holdGit puts a wrapper named git first on PATH that records every git
// command it runs and holds the first one whose arguments start with sub
// until the test writes a line to the returned fifo. It returns the fifo,
// a file that exists once a command is held, and a function that returns
// the recorded commands. It uses t.Setenv, so the test must not be
// parallel.
func holdGit(t *testing.T, sub string) (fifo, held string, commands func() []string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	held = filepath.Join(dir, "held")
	once := filepath.Join(dir, "once")
	fifo = filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quote(logPath) + "\n" +
		"if [ \"$1\" = " + quote(sub) + " ] && mkdir " + quote(once) + " 2>/dev/null; then\n" +
		"\t: > " + quote(held) + "\n" +
		"\tread -r _ < " + quote(fifo) + "\n" +
		"fi\n" +
		"exec " + quote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fifo, held, func() []string {
		b, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// R57, TUI: a load runs its git under the session's context. It is held
// in the show-ref that lists the references, the session ends there, as
// when the client disconnects, and the held git is let go: no git starts
// after that, so the default reference is not looked up.
func TestRefLoadStartsNoGitAfterSessionEnds(t *testing.T) { //nolint:paralleltest // t.Setenv
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "trunk", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	fifo, held, commands := holdGit(t, "show-ref")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	msgs := make(chan tea.Msg, 1)
	go func() { msgs <- updateRefCmd(ctx, openRepo{path: dir}, 0)() }()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(held); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git show-ref never started")
		}
	}
	// Opening the fifo for writing waits for the held git to open it.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, _ = w.WriteString("go\n")
	w.Close() //nolint:errcheck,gosec

	var msg tea.Msg
	select {
	case msg = <-msgs:
	case <-time.After(2 * time.Minute):
		t.Fatal("the load did not end after the session ended")
	}
	if _, ok := msg.(RefMsg); ok {
		t.Errorf("the load found the reference after the session ended: %v", msg)
	}
	cmds := commands()
	for i, c := range cmds {
		if strings.HasPrefix(c, "show-ref") {
			if after := cmds[i+1:]; len(after) > 0 {
				t.Errorf("git ran after the session ended: %q", after)
			}
			return
		}
	}
	t.Fatalf("no git show-ref in %q", cmds)
}
