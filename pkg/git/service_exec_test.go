package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gitRun runs git with args in dir and returns its trimmed stdout.
func gitRun(t *testing.T, dir string, stdin io.Reader, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// bareWithRefs returns a bare repository with n branches, all pointing at
// one empty commit, so that its ref advertisement is about 60 bytes per
// branch. The refs are written as packed-refs, which is much faster than
// creating them one by one.
func bareWithRefs(t *testing.T, n int) string {
	t.Helper()
	dir := initBare(t)
	tree := gitRun(t, dir, strings.NewReader(""), "mktree")
	commit := gitRun(t, dir, nil, "commit-tree", tree, "-m", "refs")
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := range n {
		fmt.Fprintf(&b, "%s refs/heads/b%07d\n", commit, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// eofAfterWriter is a client that takes its first write and then fails
// every later one with io.EOF, as an SSH channel the client closed does
// while the connection stays open.
type eofAfterWriter struct {
	writes atomic.Int32
}

func (w *eofAfterWriter) Write(p []byte) (int, error) {
	if w.writes.Add(1) > 1 {
		return 0, io.EOF
	}
	return len(p), nil
}

// R32: once the copy of git's output to the client fails, git must not stay
// blocked on a full pipe nobody reads. The context is not cancelled, as an
// SSH session's is not when only its channel closes (OpenSSH ControlMaster),
// and the input stays open: nothing but the failed copy can stop git. The
// handler must return, with git gone, long before any timeout.
func TestServiceOutputCopyFailureStopsGit(t *testing.T) {
	// 50,000 refs make an advertisement of about 3 MB, far more than a
	// pipe holds.
	dir := bareWithRefs(t, 50_000)

	ctx, cancel := context.WithCancel(t.Context())
	// On failure, stop git so the test does not leave it behind.
	t.Cleanup(cancel)
	stdin, stdinW := io.Pipe()
	t.Cleanup(func() { stdinW.Close() }) //nolint: errcheck

	var cmd *exec.Cmd
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(ctx, ServiceCommand{
			Stdin:   stdin,
			Stdout:  &eofAfterWriter{},
			Dir:     dir,
			CmdFunc: func(c *exec.Cmd) { cmd = c },
		})
	}()

	select {
	case err := <-errc:
		t.Logf("handler returned: %v", err)
	case <-time.After(waitDelay + 5*time.Second):
		t.Fatal("handler still waiting for git after the copy of its output failed")
	}
	if cmd.ProcessState == nil {
		t.Fatal("handler returned before git exited")
	}
}

// panicWriter is a client whose write panics.
type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("client write panicked") }

// R16: a panic while copying git's output to the client is recovered: the
// server keeps running, git is stopped and the handler returns. Without the
// recover the panic ends the test binary.
func TestServiceCopyPanicRecovered(t *testing.T) {
	levels := captureLog(t)
	var cmd *exec.Cmd
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(t.Context(), ServiceCommand{
			Stdout:  panicWriter{},
			Dir:     bareWithRefs(t, 10_000),
			Args:    []string{"--advertise-refs"},
			CmdFunc: func(c *exec.Cmd) { cmd = c },
		})
	}()
	t.Logf("handler returned: %v", waitErr(t, errc))
	if cmd.ProcessState == nil {
		t.Fatal("handler returned before git exited")
	}
	if level := levels()["recovered panic"]; level != "error" {
		t.Errorf("recovered panic logged at %q, want %q", level, "error")
	}
}

// packWithBlob returns a pack holding one commit with a size-byte random
// file, and the commit's ID.
func packWithBlob(t *testing.T, size int) ([]byte, string) {
	t.Helper()
	src := t.TempDir()
	gitRun(t, src, nil, "init", "-q")
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, nil, "add", "big")
	gitRun(t, src, nil, "commit", "-q", "-m", "big")
	commit := gitRun(t, src, nil, "rev-parse", "HEAD")
	cmd := exec.CommandContext(t.Context(), "git", "pack-objects", "--revs", "--stdout", "-q")
	cmd.Dir = src
	cmd.Stdin = strings.NewReader(commit + "\n")
	pack, err := cmd.Output()
	if err != nil {
		t.Fatalf("git pack-objects: %v", err)
	}
	return pack, commit
}

// quarantines returns the push quarantine directories left in the bare
// repository dir.
func quarantines(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "objects", "tmp_objdir-incoming-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// R33: a push whose client goes away mid-pack must not leave the partly
// received pack behind. git must be told the input ended, not killed: a
// killed receive-pack cannot remove its quarantine directory, and nothing
// in the server ever does.
func TestReceivePackInterruptedLeavesNoQuarantine(t *testing.T) {
	pack, commit := packWithBlob(t, 8<<20)
	dir := initBare(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdin, stdinW := io.Pipe()
	t.Cleanup(func() { stdinW.Close() }) //nolint: errcheck
	stdout, stdoutW := io.Pipe()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	errc := make(chan error, 1)
	go func() {
		errc <- ReceivePack(ctx, ServiceCommand{
			Stdin:  stdin,
			Stdout: stdoutW,
			Stderr: io.Discard,
			Dir:    dir,
			// Store the pack as it comes (index-pack), as for any push
			// of more than a few objects.
			Config: []string{"receive.unpackLimit=1"},
		})
		stdoutW.Close() //nolint: errcheck
	}()

	// Send the ref update and half of the pack, then stall without
	// closing the input, as a client that went away does.
	sent := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		line := fmt.Sprintf("%s %s refs/heads/main\x00report-status\n", strings.Repeat("0", 40), commit)
		fmt.Fprintf(&b, "%04x%s0000", len(line)+4, line)
		b.Write(pack[:len(pack)/2])
		_, err := stdinW.Write(b.Bytes())
		sent <- err
	}()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("sending the push: %v", err)
		}
	case err := <-errc:
		t.Fatalf("receive-pack returned before the push was sent: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("timed out sending the push")
	}
	deadline := time.Now().Add(testTimeout)
	for len(quarantines(t, dir)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("receive-pack made no quarantine directory")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let index-pack write some of what it has.
	time.Sleep(200 * time.Millisecond)
	cancel()

	err := waitErr(t, errc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if q := quarantines(t, dir); len(q) != 0 {
		t.Errorf("interrupted push left its quarantine directory: %v", q)
	}
}

// Cancelling the context closes git's input rather than killing git: git
// exits on its own, so the error wraps an exit status, not a kill.
func TestServiceCancelLetsGitExit(t *testing.T) {
	err := runCancelled(t, ServiceCommand{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Exited() {
		t.Fatalf("git was killed instead of exiting on its own: %v", err)
	}
	if runtime.GOOS != "windows" && strings.Contains(err.Error(), "signal") {
		t.Fatalf("git was killed instead of exiting on its own: %v", err)
	}
}
