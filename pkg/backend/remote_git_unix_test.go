//go:build !windows

package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a git first on PATH that runs script.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// waitForFile waits, generously, for path to exist.
func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s never appeared", what, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stopping a git command also stops the helpers it started (ssh,
// git-remote-https), which run in its process group: here a background
// child that records the SIGTERM it gets.
func TestRunRemoteGitTerminatesHelpers(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	marker := filepath.Join(dir, "child-terminated")
	fakeGit(t, `sh -c 'trap "echo terminated > \"$MARKER\"; exit 0" TERM; echo ready > "$READY"; while :; do sleep 0.1; done' &
wait
`)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunRemoteGit(ctx, dir, []string{"READY=" + ready, "MARKER=" + marker}, "fetch")
	}()
	waitForFile(t, ready, "the helper did not start")
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("RunRemoteGit succeeded after its context was cancelled")
		}
	case <-time.After(RemoteGitWaitDelay + time.Minute):
		t.Fatal("RunRemoteGit did not return after its context was cancelled")
	}
	waitForFile(t, marker, "the helper was not sent SIGTERM")
}

// git is told never to prompt for credentials: in a process group of its
// own it would be stopped by SIGTTIN reading a terminal rather than fail.
func TestRunRemoteGitNoPrompt(t *testing.T) {
	fakeGit(t, `echo "prompt=$GIT_TERMINAL_PROMPT" >&2
exit 1
`)
	err := RunRemoteGit(t.Context(), t.TempDir(), nil, "fetch")
	if err == nil || !strings.Contains(err.Error(), "prompt=0") {
		t.Fatalf("RunRemoteGit = %v, want git run with GIT_TERMINAL_PROMPT=0", err)
	}
}
