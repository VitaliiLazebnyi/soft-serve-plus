//go:build !windows

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSlowGit stands in for a git fetch that runs for two minutes unless it
// gets SIGTERM, which it records, as git does when it removes its lock files
// and exits.
const fakeSlowGit = `#!/bin/sh
trap 'echo terminated > "$MARKER"; exit 143' TERM
echo started > "$STARTED"
i=0
while [ $i -lt 1200 ]; do sleep 0.1; i=$((i+1)); done
`

// A shutdown stops a running mirror git command with SIGTERM, which git
// handles by removing its locks, rather than leaving it running past the
// server, or killing it outright.
func TestRunMirrorGitTerminatesOnCancel(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeSlowGit), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "terminated")
	env := []string{"STARTED=" + started, "MARKER=" + marker}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runMirrorGit(ctx, dir, env, "fetch", "--prune") }()

	// Cancel only once the command is running.
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the git command did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runMirrorGit = %v, want %v", err, context.Canceled)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runMirrorGit did not stop after its context was cancelled")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("git was not sent SIGTERM: %v", err)
	}
}
