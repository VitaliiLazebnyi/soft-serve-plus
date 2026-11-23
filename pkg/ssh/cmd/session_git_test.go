//go:build !windows

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gitFence puts a wrapper named git first on PATH that records every git
// command it runs and holds the first `git rev-parse --git-dir`, the one
// git.Open runs, until the test releases it. It uses t.Setenv, so the test
// must not be parallel.
type gitFence struct {
	log, marker, once, fifo string
}

func newGitFence(t *testing.T) *gitFence {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &gitFence{
		log:    filepath.Join(dir, "git.log"),
		marker: filepath.Join(dir, "held"),
		once:   filepath.Join(dir, "once"),
		fifo:   filepath.Join(dir, "fifo"),
	}
	if err := syscall.Mkfifo(f.fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quote(f.log) + "\n" +
		"case \" $* \" in\n" +
		"*' rev-parse --git-dir '*)\n" +
		"\tif mkdir " + quote(f.once) + " 2>/dev/null; then\n" +
		"\t\t: > " + quote(f.marker) + "\n" +
		"\t\tread -r _ < " + quote(f.fifo) + "\n" +
		"\tfi\n" +
		";;\n" +
		"esac\n" +
		"exec " + quote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// reset forgets the commands recorded so far and arms the hold again.
func (f *gitFence) reset(t *testing.T) {
	t.Helper()
	for _, p := range []string{f.log, f.marker, f.once} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

// waitHeld waits until a git command is held.
func (f *gitFence) waitHeld(t *testing.T, done <-chan error) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(f.marker); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the command ended before it ran git rev-parse --git-dir: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("git rev-parse --git-dir never started")
		}
	}
}

// commands returns the git commands recorded so far.
func (f *gitFence) commands(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// R57: every `repo` command runs its git under the session's context. The
// command is held in the git rev-parse that opens the repository, the
// session ends there, as when the client disconnects, and the held git is
// then let go. A command whose git commands are bound to the session starts
// no git after that: the next one fails to start, or the held one is killed.
// Unbound, the command would go on to read the repository after the client
// left.
//
// The test cannot tell how soon the held git ends: git-module stops every
// git it runs after one minute, bound or not, with the same kill.
func TestRepoCommandsStartNoGitAfterSessionEnds(t *testing.T) { // serial: t.Setenv
	ctx, head := viewRepo(t)
	fence := newGitFence(t)
	for _, args := range [][]string{
		{"blob", "view", "README.md"},
		{"tree", "view"},
		{"commit", "view", head},
		{"branch", "list", "view"},
		{"branch", "default", "view"},
		{"tag", "list", "view"},
		{"info", "view"},
	} {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			fence.reset(t)
			sessCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := runRepoOutput(t, sessCtx, args...)
				done <- err
			}()
			fence.waitHeld(t, done)
			// Opening the fifo for writing waits for the held git to open
			// it for reading, so the session ends while git waits on it.
			w, err := os.OpenFile(fence.fifo, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			// The held git may be dead already, which fails the write.
			_, _ = w.WriteString("go\n")
			w.Close() //nolint:errcheck

			var cmdErr error
			select {
			case cmdErr = <-done:
			case <-time.After(2 * time.Minute):
				t.Fatal("the command did not end after the session ended")
			}
			if cmdErr == nil {
				t.Error("the command succeeded after the session ended")
			}
			cmds := fence.commands(t)
			held := -1
			for i, c := range cmds {
				if strings.Contains(" "+c+" ", " rev-parse --git-dir ") {
					held = i
					break
				}
			}
			if held < 0 {
				t.Fatalf("no git rev-parse --git-dir in %q", cmds)
			}
			if after := cmds[held+1:]; len(after) > 0 {
				t.Errorf("git ran after the session ended: %q", after)
			}
		})
	}
}
