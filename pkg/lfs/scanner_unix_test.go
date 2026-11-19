//go:build !windows

package lfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
)

// A scan that panics still stops and waits for its git commands, so none
// is left running or as a zombie.
func TestSearchPointerBlobsPanicReapsGit(t *testing.T) {
	repo := newPointerRepo(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin := t.TempDir()
	pids := filepath.Join(t.TempDir(), "pids")
	script := fmt.Sprintf("#!/bin/sh\necho $$ >> %q\nexec %q \"$@\"\n", pids, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := readPointer
	readPointer = func([]byte) (Pointer, error) { panic("scanner boom") }
	t.Cleanup(func() { readPointer = orig })

	ctx := log.WithContext(context.Background(), log.New(&strings.Builder{}))
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	go SearchPointerBlobs(ctx, repo, pointerChan, errChan)
	for range pointerChan { //nolint:revive // drain until closed
	}
	if err := <-errChan; err == nil {
		t.Fatal("the panic was not reported")
	}

	data, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 2 {
		t.Fatalf("started %d git commands, want 2: %q", len(lines), lines)
	}
	for _, line := range lines {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatal(err)
		}
		// A process that exited but was not waited for is a zombie,
		// which signal 0 still finds.
		deadline := time.Now().Add(time.Minute)
		for {
			err := syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("git %d is still there after the scan returned: %v", pid, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
