package gitserve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
)

// testTimeout bounds every wait in these tests, so a handler that never
// returns fails the test instead of hanging it.
const testTimeout = time.Minute

// initBare creates an empty bare repository and returns its path.
func initBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// readLine reads one line from r, failing the test if none comes in time.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	c := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		c <- result{line, err}
	}()
	select {
	case res := <-c:
		if res.err != nil {
			t.Fatalf("reading a line: %v", res.err)
		}
		return res.line
	case <-time.After(testTimeout):
		t.Fatal("timed out reading a line")
		return ""
	}
}

// waitErr waits for the handler's error, failing the test if the handler
// does not return in time.
func waitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the service handler to return")
		return nil
	}
}

// runCancelled runs upload-pack with scmd against an empty repository, reads
// the first line it writes, then calls atCancel with that line and cancels
// the context, and returns the handler's error. Stdin stays open and empty,
// so upload-pack waits for the client after the advertisement until it is
// killed.
func runCancelled(t *testing.T, scmd ServiceCommand, atCancel func(line string)) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()
	defer stdout.Close() //nolint: errcheck

	scmd.Stdin = stdin
	scmd.Stdout = stdoutW
	scmd.Dir = initBare(t)

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(ctx, scmd)
		stdoutW.Close() //nolint: errcheck
	}()

	line := readLine(t, stdout)
	go io.Copy(io.Discard, stdout) //nolint: errcheck
	if atCancel != nil {
		atCancel(line)
	}
	cancel()

	return waitErr(t, errc)
}

// TestUploadPackChildHoldsOutputOpen covers a process started by git that
// keeps git's output open after git was killed, as the git wrapper on Windows
// does with the git it runs. The handler must still return once the context
// is cancelled instead of waiting for that process. The stand-in for git
// ignores its input, so closing it does not stop it, and it is killed after
// WaitDelay.
func TestUploadPackChildHoldsOutputOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	err = runCancelled(t, ServiceCommand{
		// Stand in for git: start a child that inherits stdout and outlives
		// the shell, print its pid, and wait.
		CmdFunc: func(c *exec.Cmd) {
			c.Path = sh
			c.Args = []string{"sh", "-c", "sleep 60 & echo $!; wait"}
			c.WaitDelay = 100 * time.Millisecond // meant to run out: the child holds stdout open
		},
	}, func(line string) {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("reading the child pid: %v", err)
		}
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill() //nolint: errcheck
			}
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestServiceBackgroundProcessHoldsOutputOpen covers a process started by
// git, such as one a hook leaves running in the background, that keeps
// git's output open after git exited normally. The handler must return
// with git's output once that output stopped coming, instead of waiting for
// the process.
func TestServiceBackgroundProcessHoldsOutputOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	levels := captureLog(t)

	var stdout, stderr syncBuffer
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdout: &stdout,
			Stderr: &stderr,
			Dir:    t.TempDir(),
			// Stand in for git: start a child that inherits stdout and
			// stderr and outlives the shell, print its pid, and exit.
			CmdFunc: func(c *exec.Cmd) {
				c.Path = sh
				c.Args = []string{"sh", "-c", "sleep 60 & echo $!; echo done >&2"}
			},
			outputIdleTimeout: 100 * time.Millisecond,
		})
	}()

	err = waitErr(t, errc)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(stdout.Bytes())))
	if perr == nil {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill() //nolint: errcheck
		}
	}
	if err != nil {
		t.Fatalf("expected the service to succeed, got %v", err)
	}
	if perr != nil {
		t.Fatalf("expected the child pid on stdout, got %q", stdout.Bytes())
	}
	if got := string(stderr.Bytes()); got != "done\n" {
		t.Fatalf("expected stderr %q, got %q", "done\n", got)
	}
	// The handler closing the pipes under the copies is expected, and must
	// not be logged as an error.
	for _, stream := range []string{"stdout", "stderr"} {
		prefix := "gitServiceHandler: failed to copy " + stream + ": "
		var found bool
		for msg, level := range levels() {
			if !strings.HasPrefix(msg, prefix) {
				continue
			}
			found = true
			if level != "debug" {
				t.Errorf("%q logged at %q, want %q", msg, level, "debug")
			}
		}
		if !found {
			t.Errorf("expected the %s copy to be stopped and logged", stream)
		}
	}
}

// slowWriter is a client that takes delay for every write.
type slowWriter struct {
	delay time.Duration
	n     atomic.Int64
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// TestServiceSlowClientGetsAllOutput covers a client that reads git's
// output slower than outputIdleTimeout per write: the handler must not
// take it for a process holding the output open, and must copy all of the
// output after git exited. Git writes everything at once and exits, so all
// of the client's reads are of the stream's tail after git is gone; with
// cmd.Stdout set to the client and WaitDelay as the limit, os/exec cuts
// the copy there (exec.ErrWaitDelay) and the client loses the end.
func TestServiceSlowClientGetsAllOutput(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	const size = 256 << 10
	w := &slowWriter{delay: 50 * time.Millisecond}
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdout: w,
			Dir:    t.TempDir(),
			CmdFunc: func(c *exec.Cmd) {
				c.Path = sh
				c.Args = []string{"sh", "-c", "head -c " + strconv.Itoa(size) + " /dev/zero"}
			},
			outputIdleTimeout: 20 * time.Millisecond,
		})
	}()

	if err := waitErr(t, errc); err != nil {
		t.Fatalf("expected the service to succeed, got %v", err)
	}
	if got := w.n.Load(); got != size {
		t.Fatalf("expected %d bytes of output, got %d", size, got)
	}
}

// TestUploadPackFailsAfterCancel covers git failing on its own after the
// context was cancelled but before the handler stopped it (here Cancel
// does nothing): the real failure must be reported, not the cancellation.
func TestUploadPackFailsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()
	defer stdout.Close() //nolint: errcheck

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(ctx, ServiceCommand{
			Stdin:  stdin,
			Stdout: stdoutW,
			Dir:    initBare(t),
			// Do not stop git when the context is cancelled, so that it
			// fails on its own.
			CmdFunc: func(c *exec.Cmd) { c.Cancel = func() error { return nil } },
		})
		stdoutW.Close() //nolint: errcheck
	}()

	readLine(t, stdout)
	go io.Copy(io.Discard, stdout) //nolint: errcheck
	cancel()

	// A malformed packet line makes upload-pack fail with a protocol error.
	if _, err := stdinW.Write([]byte("zzzz")); err != nil {
		t.Fatalf("writing to upload-pack: %v", err)
	}
	stdinW.Close() //nolint: errcheck

	err := waitErr(t, errc)
	if err == nil {
		t.Fatal("expected upload-pack to fail")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected the git failure, got the cancellation: %v", err)
	}
}

func TestStoppedByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if stoppedByContext(context.Background(), context.Canceled, true) {
		t.Error("a context that is not done must not report a cancellation")
	}
	if !stoppedByContext(ctx, context.Canceled, false) {
		t.Error("the context error must report a cancellation")
	}
	if !stoppedByContext(ctx, fmt.Errorf("wait: %w", context.Canceled), false) {
		t.Error("a wrapped context error must report a cancellation")
	}
	// A git that failed while the context was done, but that the handler
	// did not stop, failed on its own.
	if stoppedByContext(ctx, errors.New("exit status 128"), false) {
		t.Error("an unrelated error must not report a cancellation")
	}
	// Once Cancel closed git's input, git's exit, whatever its status or
	// a kill after WaitDelay, is the cancellation's doing.
	if !stoppedByContext(ctx, errors.New("exit status 128"), true) {
		t.Error("a git stopped for the context must report a cancellation")
	}
}

// TestServiceErrorCarriesStderr checks a git failure reports why git failed,
// from its stderr, whether or not the caller takes stderr too.
func TestServiceErrorCarriesStderr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stderr *strings.Builder
	}{
		{"no caller stderr", nil},
		{"caller stderr", &strings.Builder{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
			defer cancel()
			scmd := ServiceCommand{
				Stdout: io.Discard,
				// An empty directory is not a repository.
				Dir:  t.TempDir(),
				Args: []string{"--stateless-rpc", "--advertise-refs"},
			}
			if tc.stderr != nil {
				scmd.Stderr = tc.stderr
			}
			err := UploadPackService.Handler(ctx, scmd)
			if err == nil {
				t.Fatal("upload-pack succeeded outside a repository")
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("error does not wrap the exit error: %v", err)
			}
			if !strings.Contains(err.Error(), "fatal:") {
				t.Errorf("error does not carry git's stderr: %v", err)
			}
			if tc.stderr != nil && !strings.Contains(tc.stderr.String(), "fatal:") {
				t.Errorf("caller did not get git's stderr: %q", tc.stderr.String())
			}
		})
	}
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()
	b := &tailBuffer{max: 20}
	if got := b.String(); got != "" {
		t.Errorf("empty buffer = %q", got)
	}
	_, _ = fmt.Fprint(b, "\n  warning: one \n\n")
	if got, want := b.String(), "warning: one"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	n, err := fmt.Fprint(b, "fatal: two\nend\n")
	if err != nil || n != 15 {
		t.Errorf("Write = %d, %v; want 15, nil", n, err)
	}
	if got, want := b.String(), "...ne; fatal: two; end"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// A panic in a copy of git's output is logged on the command's Logger, the
// one its transport passes, and not on the default logger.
func TestServiceLogsPanicOnCommandLogger(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	logger := log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})
	err := UploadPackService.Handler(t.Context(), ServiceCommand{
		Stdout: panicWriter{},
		Dir:    bareWithRefs(t, 1),
		Args:   []string{"--advertise-refs"},
		Logger: logger,
	})
	if err != nil {
		t.Logf("handler: %v", err)
	}
	if !strings.Contains(string(logs.Bytes()), "client write panicked") {
		t.Fatalf("the panic was not logged on the command's logger; it logged %q", logs.Bytes())
	}
}

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

// Once the copy of git's output to the client fails, git must not stay
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
		errc <- UploadPackService.Handler(ctx, ServiceCommand{
			Stdin:   stdin,
			Stdout:  &eofAfterWriter{},
			Dir:     dir,
			CmdFunc: func(c *exec.Cmd) { cmd = c },
		})
	}()

	select {
	case err := <-errc:
		// The client going away is not a git failure: callers log
		// context.Canceled at Debug and report nothing to the client.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(waitDelay + time.Minute):
		t.Fatal("handler still waiting for git after the copy of its output failed")
	}
	if cmd.ProcessState == nil {
		t.Fatal("handler returned before git exited")
	}
}

// A git that fails for its own reason after a write to the client failed
// is a failure, reported with its stderr, not the client going away: only
// a git that died of the broken pipe is.
func TestServiceFailureAfterClientGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	err = UploadPackService.Handler(t.Context(), ServiceCommand{
		Stdout: failingWriter{err: io.EOF},
		Dir:    t.TempDir(),
		// Stand in for git: write once (the client write fails), then fail
		// on its own.
		CmdFunc: func(c *exec.Cmd) {
			c.Path = sh
			c.Args = []string{"sh", "-c", "printf x; sleep 0.3; echo 'fatal: disk full' >&2; exit 3"}
		},
	})
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a git failure, not a cancellation", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	if err == nil || !strings.Contains(err.Error(), "fatal: disk full") {
		t.Errorf("err = %v, want git's stderr in it", err)
	}
}

// git for Windows dies of a closed output with die()'s 128 and "fwrite
// error: Invalid argument", not of SIGPIPE or 141; after a failed client
// write that is the client going away. With another message, 128 is still
// a git failure.
func TestServiceClientGoneExit128(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	for _, tc := range []struct {
		msg      string
		canceled bool
	}{
		{"fatal: fwrite error: Invalid argument", true},
		{"fatal: write error: Broken pipe", true},
		{"fatal: write failure on 'standard output': Invalid argument", true},
		{"fatal: fflush error: Invalid argument", true},
		{"fatal: disk full", false},
		{"fatal: sha1 file 'objects/pack/tmp_pack_x' write error. Out of diskspace", false},
	} {
		err := UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdout: failingWriter{err: io.EOF},
			Dir:    t.TempDir(),
			CmdFunc: func(c *exec.Cmd) {
				c.Path = sh
				c.Args = []string{"sh", "-c", "printf x; sleep 0.3; echo \"$0\" >&2; exit 128", tc.msg}
			},
		})
		if got := errors.Is(err, context.Canceled); got != tc.canceled {
			t.Errorf("%q: err = %v, cancellation = %v, want %v", tc.msg, err, got, tc.canceled)
		}
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%q: err = %v, want git's stderr in it", tc.msg, err)
		}
	}
}

func TestOutputClosedExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	exit := func(code string) error {
		return exec.CommandContext(t.Context(), "sh", "-c", "exit "+code).Run()
	}
	for _, tc := range []struct {
		err    error
		stderr string
		want   bool
	}{
		{exit("141"), "", true},
		{exit("128"), "fatal: fwrite error: Invalid argument", true},
		{exit("128"), "fatal: fflush error: Invalid argument", true},
		{exit("128"), "fatal: write error: Invalid argument", true},
		{exit("128"), "fatal: write failure on 'standard output': Invalid argument", true},
		{exit("128"), "fatal: packet write failed: Invalid argument", true},
		{exit("128"), "fatal: packet write with format failed", true},
		{exit("128"), "erreur fatale : write error: Broken pipe", true},
		{exit("128"), "fatal: the remote end hung up: Broken pipe", true},
		{exit("128"), "fatal: not a git repository", false},
		{exit("128"), "fatal: sha1 file 'objects/pack/tmp_pack_x' write error. Out of diskspace", false},
		{exit("128"), "fatal: packet write failed - data exceeds max packet size", false},
		{exit("128"), "fatal: unable to write new index file", false},
		{exit("3"), "fatal: fwrite error: Invalid argument", false},
		{errors.New("other"), "fatal: write error", false},
	} {
		if got := outputClosedExit(tc.err, tc.stderr); got != tc.want {
			t.Errorf("outputClosedExit(%v, %q) = %v, want %v", tc.err, tc.stderr, got, tc.want)
		}
	}
}

func TestBrokenPipeExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	for _, tc := range []struct {
		script string
		want   bool
	}{
		{"kill -PIPE $$", true},
		{"exit 141", true},
		{"exit 3", false},
		{"kill -TERM $$", false},
	} {
		err := exec.CommandContext(t.Context(), "sh", "-c", tc.script).Run()
		if got := brokenPipeExit(err); got != tc.want {
			t.Errorf("%q: brokenPipeExit(%v) = %v, want %v", tc.script, err, got, tc.want)
		}
	}
	if brokenPipeExit(errors.New("other")) {
		t.Error("brokenPipeExit of a non-exit error = true")
	}
}

// A CmdFunc that sets git's output is refused rather than silently
// replaced: the handler owns git's output.
func TestServiceCmdFuncOutputRefused(t *testing.T) {
	err := UploadPackService.Handler(t.Context(), ServiceCommand{
		Stdout:  io.Discard,
		Dir:     initBare(t),
		CmdFunc: func(c *exec.Cmd) { c.Stdout = io.Discard },
	})
	if err == nil || !strings.Contains(err.Error(), "stdout already set") {
		t.Fatalf("err = %v, want stdout already set", err)
	}
}

// A CmdFunc that sets git's input while the command has one is refused by
// StdinPipe, which runs after CmdFunc and the output pipes, and the output
// pipes made by then are closed.
func TestServiceCmdFuncStdinRefused(t *testing.T) {
	err := UploadPackService.Handler(t.Context(), ServiceCommand{
		Stdin:   strings.NewReader(""),
		Stdout:  io.Discard,
		Dir:     initBare(t),
		CmdFunc: func(c *exec.Cmd) { c.Stdin = strings.NewReader("") },
	})
	if err == nil || !strings.Contains(err.Error(), "Stdin already set") {
		t.Fatalf("err = %v, want Stdin already set", err)
	}
}

// panicWriter is a client whose write panics.
type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("client write panicked") }

// A panic while copying git's output to the client is recovered: the
// server keeps running, git is stopped and the handler returns. Without the
// recover the panic ends the test binary.
func TestServiceCopyPanicRecovered(t *testing.T) {
	levels := captureLog(t)
	var cmd *exec.Cmd
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
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

// quarantineBytes returns how many bytes the files in the quarantine
// directories of the bare repository dir hold.
func quarantineBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, q := range quarantines(t, dir) {
		// A file can go while the walk runs; what is left still counts.
		_ = filepath.WalkDir(q, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // see above
			}
			// The size from the open file, not from the directory
			// listing: NTFS updates a listed size lazily, so the
			// pack index-pack is writing can be listed as empty.
			f, err := os.Open(path)
			if err != nil {
				return nil // see above
			}
			defer f.Close() //nolint: errcheck
			if info, err := f.Stat(); err == nil {
				n += info.Size()
			}
			return nil
		})
	}
	return n
}

// A push whose client goes away mid-pack must not leave the partly
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
		errc <- ReceivePackService.Handler(ctx, ServiceCommand{
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
	// Cancel once index-pack has written some of what it has into the
	// quarantine directory.
	deadline := time.Now().Add(testTimeout)
	for quarantineBytes(t, dir) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("receive-pack wrote nothing into a quarantine directory")
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// failingWriter is a client connection that fails every write. When cancel
// is set it is called first, the way a client going away cancels the
// request context.
type failingWriter struct {
	err    error
	cancel context.CancelFunc
}

func (w failingWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	return 0, w.err
}

// syncBuffer is a bytes.Buffer safe for concurrent use, for a logger used
// by goroutines the test does not wait for.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// captureLog swaps the default logger for one that records every level as
// JSON, and returns the recorded entries' levels by message.
func captureLog(t *testing.T) func() map[string]string {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Default()
	log.SetDefault(log.NewWithOptions(buf, log.Options{
		Level:     log.DebugLevel,
		Formatter: log.JSONFormatter,
	}))
	t.Cleanup(func() { log.SetDefault(prev) })
	return func() map[string]string {
		levels := map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for {
			var entry struct{ Level, Msg string }
			if err := dec.Decode(&entry); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("decoding log: %v", err)
			}
			levels[entry.Msg] = entry.Level
		}
		return levels
	}
}

// A client that goes away makes copying git's output to it fail. That is
// expected and must not be logged as an error; any other copy failure still
// is.
func TestGitServiceOutputCopyErrorLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	writeErr := errors.New("write failed")
	for _, tc := range []struct {
		name      string
		err       error
		cancelCtx bool
		want      string
	}{
		{"context cancelled", writeErr, true, "debug"},
		{"ssh channel closed", io.EOF, false, "debug"},
		{"closed pipe", io.ErrClosedPipe, false, "debug"},
		{"closed connection", fmt.Errorf("write: %w", net.ErrClosed), false, "debug"},
		{"broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, false, "debug"},
		{"connection reset", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.ECONNRESET)}, false, "debug"},
		{"other failure", writeErr, false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := captureLog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			w := failingWriter{err: tc.err}
			if tc.cancelCtx {
				w.cancel = cancel
			}
			// The result does not matter here, only how the copy failure
			// is logged.
			_ = UploadPackService.Handler(ctx, ServiceCommand{Stdout: w, Dir: dir})

			msg := "gitServiceHandler: failed to copy stdout: " + tc.err.Error()
			if got := levels()[msg]; got != tc.want {
				t.Errorf("%q logged at %q, want %q", msg, got, tc.want)
			}
		})
	}
}

// failingReader is a client connection that fails every read. When cancel
// is set it is called first, the way a client going away cancels the
// request context.
type failingReader struct {
	err    error
	cancel context.CancelFunc
}

func (r failingReader) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return 0, r.err
}

// garbageReader is a client that sends bytes git does not accept, without
// end.
type garbageReader struct{}

func (garbageReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

// waitForLog waits for the stdin copy, which the handler does not wait
// for, to log its failure, and returns the entry's message and level.
func waitForLog(t *testing.T, levels func() map[string]string, prefix string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		for msg, level := range levels() {
			if strings.HasPrefix(msg, prefix) {
				return msg, level
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log entry starting with %q", prefix)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Copying the client's input to git fails when the client goes away, or
// when git exits before reading all of it. Both are expected and must not
// be logged as an error; any other copy failure still is.
func TestGitServiceInputCopyErrorLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	const prefix = "gitServiceHandler: failed to copy stdin: "
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		name      string
		stdin     io.Reader
		cancelCtx bool
		want      string
	}{
		// git rejects the input and exits while the client keeps sending,
		// so writing to git fails with a broken pipe, or with
		// os.ErrClosed once the pipe was closed after git exited.
		{"git exited", garbageReader{}, false, "debug"},
		{"context cancelled", failingReader{err: readErr}, true, "debug"},
		{"closed pipe", failingReader{err: io.ErrClosedPipe}, false, "debug"},
		{"closed connection", failingReader{err: fmt.Errorf("read: %w", net.ErrClosed)}, false, "debug"},
		{"connection reset", failingReader{err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, false, "debug"},
		{"other failure", failingReader{err: readErr}, false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := captureLog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stdin := tc.stdin
			if r, ok := stdin.(failingReader); ok && tc.cancelCtx {
				r.cancel = cancel
				stdin = r
			}
			// The result does not matter here, only how the copy failure
			// is logged.
			_ = UploadPackService.Handler(ctx, ServiceCommand{Stdin: stdin, Dir: dir})

			if msg, level := waitForLog(t, levels, prefix); level != tc.want {
				t.Errorf("%q logged at %q, want %q", msg, level, tc.want)
			}
		})
	}
}

// The copy to git's input fails with os.ErrClosed once git exited and its
// input pipe was closed; that is expected.
func TestGitGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"file already closed", &os.PathError{Op: "write", Path: "|1", Err: os.ErrClosed}, true},
		{"other failure", errors.New("write failed"), false},
	} {
		if got := gitGone(tc.err); got != tc.want {
			t.Errorf("%s: gitGone(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// On Windows a reset or aborted connection and a closed pipe come as
// Windows error codes, not as syscall.ECONNRESET or syscall.EPIPE. They
// mean the client went away there, and only there: on other systems the
// same numbers are unrelated errors or none.
func TestClientGoneWindowsErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"WSAECONNRESET", 10054},
		{"WSAECONNABORTED", 10053},
		{"ERROR_BROKEN_PIPE", 109},
		{"ERROR_NO_DATA", 232},
	} {
		for _, err := range []error{
			tc.errno,
			&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("wsasend", tc.errno)},
			&os.PathError{Op: "write", Path: "|1", Err: tc.errno},
		} {
			if !clientGoneOn("windows", err) {
				t.Errorf("%s: clientGoneOn(windows, %v) = false, want true", tc.name, err)
			}
			for _, goos := range []string{"linux", "darwin"} {
				if clientGoneOn(goos, err) {
					t.Errorf("%s: clientGoneOn(%s, %v) = true, want false", tc.name, goos, err)
				}
			}
		}
	}
	if clientGoneOn("windows", syscall.Errno(10061)) { // WSAECONNREFUSED
		t.Error("clientGoneOn(windows, WSAECONNREFUSED) = true, want false")
	}
}

// stallingReader is a client that sends bytes git rejects, so git exits,
// and then sends nothing more without going away: every later read blocks
// until it is closed, or a read deadline is set on it when it takes one.
type stallingReader struct {
	sent    atomic.Bool
	reading atomic.Bool
	stopped atomic.Bool
	once    sync.Once
	stop    chan struct{}
}

func newStallingReader() *stallingReader {
	return &stallingReader{stop: make(chan struct{})}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if !r.sent.Swap(true) {
		return copy(p, "zzzz"), nil
	}
	r.reading.Store(true)
	defer r.reading.Store(false)
	<-r.stop
	return 0, os.ErrDeadlineExceeded
}

// close ends a blocked read.
func (r *stallingReader) close() {
	r.once.Do(func() { close(r.stop) })
}

// deadlineStallingReader is a stallingReader whose reads a read deadline
// ends, as a network connection's or an HTTP request body's.
type deadlineStallingReader struct {
	*stallingReader
}

func (r deadlineStallingReader) SetReadDeadline(t time.Time) error {
	if !t.After(time.Now()) {
		r.stopped.Store(true)
		r.close()
	}
	return nil
}

// When git exits before the client is done, the handler must not return
// while the copy still reads the client's input: an input that takes a
// read deadline gets one that ends the read, and the copy is waited for.
func TestGitServiceJoinsStdinCopy(t *testing.T) {
	r := newStallingReader()
	t.Cleanup(r.close)

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdin: deadlineStallingReader{r},
			Dir:   dir,
		})
	}()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}

	if !r.stopped.Load() {
		t.Error("the read of the client's input was not ended by a read deadline")
	}
	if r.reading.Load() {
		t.Error("the handler returned while the client's input was still being read")
	}
}

// An input that takes no read deadline, such as an SSH channel, cannot be
// interrupted; the handler must not wait for its read, which only ends once
// the input's owner closes it after the handler returns.
func TestGitServiceLeavesUninterruptibleStdin(t *testing.T) {
	r := newStallingReader()
	t.Cleanup(r.close)

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdin: r,
			Dir:   dir,
		})
	}()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}
}

// flushRecorder is the client's output; flushed is closed at the first
// Flush.
type flushRecorder struct {
	once    sync.Once
	flushed chan struct{}
}

func (w *flushRecorder) Write(p []byte) (int, error) { return len(p), nil }

func (w *flushRecorder) Flush() { w.once.Do(func() { close(w.flushed) }) }

// An input that takes no read deadline is waited for when the command asks
// for it, as an HTTP request body must not be read after the handler
// returns: the output is flushed, and the handler returns only once the
// client is done sending.
func TestGitServiceJoinsUninterruptibleStdinWhenAsked(t *testing.T) {
	r := newStallingReader()
	t.Cleanup(r.close)
	out := &flushRecorder{flushed: make(chan struct{})}

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdin:     r,
			Stdout:    out,
			Dir:       dir,
			JoinStdin: true,
		})
	}()

	select {
	case <-out.flushed:
	case err := <-errc:
		t.Fatalf("the handler returned (%v) while the client's input was still being read", err)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the output to be flushed")
	}
	// The handler waits for the input once git has exited; until it is
	// there, it must not have returned.
	deadline := time.Now().Add(testTimeout)
	for !joiningStdin() {
		select {
		case err := <-errc:
			t.Fatalf("the handler returned (%v) while the client's input was still being read", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the handler did not wait for the client's input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-errc:
		t.Fatalf("the handler returned (%v) while the client's input was still being read", err)
	default:
	}

	// The client is done sending.
	r.close()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}
	if r.reading.Load() {
		t.Error("the handler returned while the client's input was still being read")
	}
}

// joiningStdin reports whether a git service handler waits, once git has
// exited, for the copy of its input to end.
func joiningStdin() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, stack, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[chan receive") && strings.Contains(stack, "github.com/charmbracelet/soft-serve/pkg/gitserve.joinStdin(") {
			return true
		}
	}
	return false
}

// resettingStallingReader is a deadlineStallingReader that, like the git
// daemon's connection, sets its own deadline before a read: the first
// deadline in the past is replaced before it ends the read.
type resettingStallingReader struct {
	*stallingReader
	deadlines atomic.Int32
}

func (r *resettingStallingReader) SetReadDeadline(t time.Time) error {
	if !t.After(time.Now()) && r.deadlines.Add(1) > 1 {
		r.stopped.Store(true)
		r.close()
	}
	return nil
}

// A read deadline that the input replaces with its own before the read
// blocks again must not leave the handler waiting: the deadline is set
// again until the copy ends.
func TestGitServiceRearmsStdinDeadline(t *testing.T) {
	r := &resettingStallingReader{stallingReader: newStallingReader()}
	t.Cleanup(r.close)

	dir := initBare(t)
	errc := make(chan error, 1)
	go func() {
		errc <- UploadPackService.Handler(t.Context(), ServiceCommand{
			Stdin: r,
			Dir:   dir,
		})
	}()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("upload-pack accepted garbage input")
	}

	if !r.stopped.Load() {
		t.Error("the read of the client's input was not ended by a read deadline")
	}
	if r.reading.Load() {
		t.Error("the handler returned while the client's input was still being read")
	}
}
