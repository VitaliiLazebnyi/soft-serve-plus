package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/safego"
)

// Service is a Git daemon service.
type Service string

const (
	// UploadPackService is the upload-pack service.
	UploadPackService Service = "git-upload-pack"
	// UploadArchiveService is the upload-archive service.
	UploadArchiveService Service = "git-upload-archive"
	// ReceivePackService is the receive-pack service.
	ReceivePackService Service = "git-receive-pack"
	// LFSTransferService is the LFS transfer service.
	LFSTransferService Service = "git-lfs-transfer"
	// LFSAuthenticateService is the LFS authenticate service.
	LFSAuthenticateService Service = "git-lfs-authenticate"
)

// String returns the string representation of the service.
func (s Service) String() string {
	return string(s)
}

// Name returns the name of the service.
func (s Service) Name() string {
	return strings.TrimPrefix(s.String(), "git-")
}

// Handler is the service handler.
func (s Service) Handler(ctx context.Context, cmd ServiceCommand) error {
	switch s {
	case UploadPackService, UploadArchiveService, ReceivePackService:
		return gitServiceHandler(ctx, s, cmd)
	case LFSTransferService:
		return LFSTransfer(ctx, cmd)
	case LFSAuthenticateService:
		return LFSAuthenticate(ctx, cmd)
	default:
		return fmt.Errorf("unsupported service: %s", s)
	}
}

// gitServiceHandler is the default service handler using the git binary.
// The SSH server, the HTTP smart protocol and the git daemon all run git
// through it.
//
// os/exec owns git's input and its life: Stdin goes through StdinPipe, and
// when the context is done (the client went away, or the server stops),
// Cancel closes that pipe instead of killing git. git then sees the end of
// its input and exits on its own, so a receive-pack removes its quarantine
// directory and waits for its helpers; a killed one leaves the partly
// received pack on disk and orphans its helpers. WaitDelay kills git if it
// has not exited that long after Cancel.
//
// git's output goes through pipes the handler copies itself, not through
// cmd.Stdout set to the client's writer. With a writer os/exec would wait at
// most WaitDelay after git exits for its copy and then cut it, so a slow
// client still receiving the end of a fetch would lose the tail. The handler
// instead waits as long as the copy makes progress (see waitOutput). A copy
// whose write to the client fails closes its pipe, so git's next write fails
// (SIGPIPE) rather than blocking on a full pipe nobody reads, which matters
// where the context does not end with the client: an SSH session's context
// is its connection's, which outlives a channel the client closed.
func gitServiceHandler(ctx context.Context, svc Service, scmd ServiceCommand) error {
	logger := log.FromContext(ctx)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Dir = scmd.Dir
	cmd.Args = append(cmd.Args, []string{
		// Enable partial clones
		"-c", "uploadpack.allowFilter=true",
		// Enable push options
		"-c", "receive.advertisePushOptions=true",
		// Disable LFS filters
		"-c", "filter.lfs.required=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.clean=",
	}...)
	for _, kv := range scmd.Config {
		cmd.Args = append(cmd.Args, "-c", kv)
	}
	cmd.Args = append(cmd.Args, svc.Name())
	cmd.Args = append(cmd.Args, scmd.Args...)
	cmd.Args = append(cmd.Args, ".")
	cmd.Env = serviceEnv(os.Environ(), scmd.Env)

	// stdin is git's input pipe, set below once the output pipes are.
	var stdin io.WriteCloser
	// cancelled records that the context stopped git, so that its exit,
	// whatever its status, is reported as the cancellation.
	var cancelled atomic.Bool
	cmd.Cancel = func() error {
		cancelled.Store(true)
		if stdin != nil {
			stdin.Close() //nolint: errcheck
		}
		return nil
	}
	cmd.WaitDelay = waitDelay

	if scmd.CmdFunc != nil {
		scmd.CmdFunc(cmd)
	}

	// The end of git's stderr is kept to explain a failure, whether or not
	// the caller wants it too.
	errTail := &tailBuffer{max: stderrTailSize}
	stderr := io.Writer(errTail)
	if scmd.Stderr != nil {
		stderr = io.MultiWriter(errTail, scmd.Stderr)
	}
	outs, err := outputPipes(cmd, scmd.Stdout, stderr)
	if err != nil {
		return err
	}
	if scmd.Stdin != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			for _, o := range outs {
				o.r.Close() //nolint: errcheck
				o.w.Close() //nolint: errcheck
			}
			return err
		}
	}

	err = cmd.Start()
	// git has its own copies of the write ends now, and the copies of its
	// output only see the end of it once every copy is closed.
	for _, o := range outs {
		o.w.Close() //nolint: errcheck
	}
	if err != nil {
		outs.close()
		if errors.Is(err, os.ErrNotExist) {
			return ErrInvalidRepo
		}
		return err
	}

	var stdinDone chan struct{}
	var interrupted atomic.Bool
	if scmd.Stdin != nil {
		stdinDone = make(chan struct{})
		safego.Go(logger, func() {
			defer close(stdinDone)
			defer stdin.Close() //nolint: errcheck
			if _, err := io.Copy(stdin, scmd.Stdin); err != nil {
				logCopyError(ctx, "stdin", err, gitGone(err) || interrupted.Load())
			}
		})
	}

	var progress outputProgress
	var wg sync.WaitGroup
	for _, o := range outs {
		// The copy is done only once a panic in it was logged.
		wg.Go(func() {
			safego.Run(logger, func() { //nolint: errcheck // Run logs a panic.
				// Closing the pipe when the copy ends, also when its
				// write to the client failed or panicked, makes git's
				// next write to it fail instead of blocking.
				defer o.r.Close() //nolint: errcheck
				if _, err := io.Copy(progress.writer(o.dst), o.r); err != nil {
					logCopyError(ctx, o.name, err, progress.stopped.Load())
				}
			})
		})
	}
	copied := make(chan struct{})
	go func() {
		wg.Wait()
		close(copied)
	}()

	err = cmd.Wait()
	// Ensure all of git's output is written before returning. Stdin is
	// joined after that, see joinStdin.
	waitOutput(ctx, copied, &progress, outs.close)
	if stdinDone != nil {
		joinStdin(scmd, stdinDone, &interrupted)
	}
	switch {
	case err == nil:
		return nil
	case stoppedByContext(ctx, err, cancelled.Load()):
		// The context was cancelled, usually because the client went away,
		// and git was stopped for it. Report the cancellation rather than
		// how git ended so callers can tell it apart from a git failure.
		return fmt.Errorf("%s: %w (%w)", svc, ctx.Err(), err)
	case progress.clientGone.Load() && outputClosedExit(err, errTail.lastLine()):
		// A write to the client failed because the client closed its end,
		// and git, whose output then went nowhere, died of it. The context
		// need not end with the client (an SSH session's is its
		// connection's), so report this as a cancellation as well. A git
		// that failed some other way after that is a failure, below; see
		// outputClosedExit for how the two are told apart and what that
		// misses.
		if msg := errTail.String(); msg != "" {
			return fmt.Errorf("%s: client went away: %w (%w): %s", svc, context.Canceled, err, msg)
		}
		return fmt.Errorf("%s: client went away: %w (%w)", svc, context.Canceled, err)
	default:
		if msg := errTail.String(); msg != "" {
			return fmt.Errorf("%s: %w: %s", svc, err, msg)
		}
		return fmt.Errorf("%s: %w", svc, err)
	}
}

// stdinInterruptInterval is how often joinStdin sets a read deadline in the
// past on the client's input until the copy of it ends.
const stdinInterruptInterval = 10 * time.Millisecond

// readDeadliner is an input whose blocked reads can be ended by a read
// deadline, such as a network connection.
type readDeadliner interface {
	SetReadDeadline(t time.Time) error
}

// joinStdin waits for the copy of the client's input to git, which done
// reports, to finish. Git has exited and Wait closed its input, so the copy
// ends at its next read, but that read can wait on the client for as long
// as the client likes.
//
// An input that takes a read deadline, such as the git daemon's connection,
// is given one in the past to end the read now, and the copy is waited for.
// The deadline is set again until the copy ends, in case the input set its
// own deadline for the read in the meantime. The input can no longer be
// read after that, which does not matter as git is gone and the caller only
// closes it.
//
// An input that cannot be interrupted is waited for only when the command
// asks for it with JoinStdin, as an HTTP request body must not be read after
// the handler returns. Its output is flushed first, so the client has the
// whole response while the handler waits for it to finish sending. A read
// deadline is not used there: on an HTTP/1 connection a deadline can also
// end the server's own background read once the body is done, which
// cancels the context of this and every later request on the connection.
//
// Any other input, such as an SSH channel, is left to its owner: the read
// ends when the owner closes it after the handler returns. Waiting there
// could deadlock, as the client waits for the exit status first.
func joinStdin(scmd ServiceCommand, done <-chan struct{}, interrupted *atomic.Bool) {
	select {
	case <-done:
		return
	default:
	}
	if d, ok := scmd.Stdin.(readDeadliner); ok {
		interrupted.Store(true)
		// The input can set its own deadline before each read, as the git
		// daemon's connection does for its idle timeout. One that does so
		// right after this deadline was set replaces it, and the read
		// blocks again, so the deadline is set again until the copy ends.
		ticker := time.NewTicker(stdinInterruptInterval)
		defer ticker.Stop()
		for {
			if err := d.SetReadDeadline(time.Now()); err != nil {
				return
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}
	if !scmd.JoinStdin {
		return
	}
	if f, ok := scmd.Stdout.(interface{ Flush() }); ok {
		f.Flush()
	}
	<-done
}

// stderrTailSize is how much of the end of git's stderr a failure reports.
const stderrTailSize = 4 << 10

// tailBuffer is a writer that keeps only the last max bytes written to it.
// Git prints why it failed last, so the end of its stderr is what explains
// a failure.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.truncated = true
	}
	return n, nil
}

// lastLine returns the last non-empty line kept, trimmed: the message git
// died with, if it did.
func (b *tailBuffer) lastLine() string {
	lines := strings.Split(strings.TrimSpace(string(b.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// String returns the non-empty lines kept, trimmed and joined with "; " so
// that they fit on one log line.
func (b *tailBuffer) String() string {
	var lines []string
	for _, line := range strings.Split(string(b.buf), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	s := strings.Join(lines, "; ")
	if s != "" && b.truncated {
		s = "..." + s
	}
	return s
}

// output is one of git's output streams, copied from the read end of its
// pipe, r, to the client, dst. git writes to w.
type output struct {
	name string
	dst  io.Writer
	r, w *os.File
}

// outputs are git's output streams.
type outputs []output

// outputPipes connects stdout and stderr, those that are set, to the
// output of cmd through new pipes.
func outputPipes(cmd *exec.Cmd, stdout, stderr io.Writer) (outputs, error) {
	var outs outputs
	for _, o := range []struct {
		name string
		dst  io.Writer
		set  *io.Writer
	}{
		{"stdout", stdout, &cmd.Stdout},
		{"stderr", stderr, &cmd.Stderr},
	} {
		if o.dst == nil {
			continue
		}
		if *o.set != nil {
			// A CmdFunc set it; the handler owns git's output.
			for _, o := range outs {
				o.r.Close() //nolint: errcheck
				o.w.Close() //nolint: errcheck
			}
			return nil, fmt.Errorf("exec: %s already set", o.name)
		}
		r, w, err := os.Pipe()
		if err != nil {
			for _, o := range outs {
				o.r.Close() //nolint: errcheck
				o.w.Close() //nolint: errcheck
			}
			return nil, err
		}
		*o.set = w
		outs = append(outs, output{name: o.name, dst: o.dst, r: r, w: w})
	}
	return outs, nil
}

// close closes the read ends, which ends copies still reading from them.
// Closing them again does nothing.
func (outs outputs) close() {
	for _, o := range outs {
		o.r.Close() //nolint: errcheck
	}
}

// outputProgress tracks the copies of git's output to the client, to tell
// whether they still make progress.
type outputProgress struct {
	// writes counts the writes to the client that finished.
	writes atomic.Int64
	// writing counts the writes to the client in progress.
	writing atomic.Int64
	// stopped is set once the handler stops waiting for the copies and
	// closes the pipes under them. Their reads then fail, which is
	// expected.
	stopped atomic.Bool
	// clientGone is set when a write to the client failed because the
	// client closed its end (see clientGone).
	clientGone atomic.Bool
}

// writer returns w, recording the writes to it in p.
func (p *outputProgress) writer(w io.Writer) io.Writer {
	return progressWriter{w: w, p: p}
}

// progressWriter is a writer whose writes are recorded in an
// outputProgress.
type progressWriter struct {
	w io.Writer
	p *outputProgress
}

func (w progressWriter) Write(b []byte) (int, error) {
	w.p.writing.Add(1)
	defer func() {
		w.p.writes.Add(1)
		w.p.writing.Add(-1)
	}()
	n, err := w.w.Write(b)
	if err != nil && clientGone(err) && !w.p.stopped.Load() {
		w.p.clientGone.Store(true)
	}
	return n, err
}

// outputIdleTimeout is how long, after git exited, the copies of its output
// may go without writing anything to the client before the handler stops
// waiting for them. It is a variable for tests.
var outputIdleTimeout = waitDelay

// waitOutput waits, after git exited, for the copies of its output to the
// client to finish. What git wrote is in the pipes by then, but a process
// git started can hold the pipes open after git is gone: a hook that leaves
// a process running in the background, or the git wrapper on Windows when
// git was killed. The copies then never finish on their own, so waitOutput
// stops waiting once the context is done, since the output has nowhere to
// go, or once the copies went outputIdleTimeout without writing to the
// client, and calls closeOutput to close the pipes and end them.
//
// A slow client does not count as idle while a write to it is in progress,
// and the next read of a pipe that still holds git's output returns at
// once, so a client that keeps receiving gets all of git's output however
// slow it is. This is the difference from os/exec's own copy, which
// WaitDelay cuts at a fixed time after git exits.
//
// outputIdleTimeout (5 s) is the handler's own bound on a copy that makes
// no progress after git exited; it is separate from the transports' idle
// timeouts, which govern the connection itself. While a write to the
// client is in progress the wait is bounded only by the context (and so by
// those transport timeouts), as it already was while git ran.
func waitOutput(ctx context.Context, copied <-chan struct{}, p *outputProgress, closeOutput func()) {
	timer := time.NewTimer(outputIdleTimeout)
	defer timer.Stop()
	last := p.writes.Load()
	for {
		select {
		case <-copied:
			return
		case <-ctx.Done():
		case <-timer.C:
			if n := p.writes.Load(); n != last || p.writing.Load() > 0 {
				last = n
				timer.Reset(outputIdleTimeout)
				continue
			}
		}
		p.stopped.Store(true)
		closeOutput()
		<-copied
		return
	}
}

// logCopyError logs a failure to copy between git and the client. A client
// that goes away, closing the SSH channel or the HTTP connection, makes the
// copy fail: either the write to (or read from) the client fails, or the
// context is cancelled for it and the pipes are closed under the copy. That
// is expected, so it is logged at Debug, as is any failure the caller marks
// as expected; any other failure is logged at Error.
func logCopyError(ctx context.Context, stream string, err error, expected bool) {
	if expected || ctx.Err() != nil || clientGone(err) {
		log.Debugf("gitServiceHandler: failed to copy %s: %v", stream, err)
		return
	}
	log.Errorf("gitServiceHandler: failed to copy %s: %v", stream, err)
}

// gitGone reports whether err, returned by a write of the client's input to
// git, means git no longer reads it. Git can exit before the client is done
// sending, for example after a protocol error. The write then fails with a
// broken pipe, which clientGone covers, or with os.ErrClosed once the pipe
// was closed after git exited or when the context was cancelled.
func gitGone(err error) bool {
	return errors.Is(err, os.ErrClosed)
}

// clientGone reports whether err, returned by a write to the client or a
// read from it, means the client closed its end. A closed SSH channel
// returns io.EOF, and a closed network connection a closed, broken pipe or
// reset error. A broken pipe also covers git's input once git exited.
func clientGone(err error) bool {
	return clientGoneOn(runtime.GOOS, err)
}

// clientGoneOn is clientGone for the operating system goos. On Windows,
// syscall.EPIPE and syscall.ECONNRESET are values Go made up, which the
// system never returns, so the Windows error codes are checked as well.
func clientGoneOn(goos string, err error) bool {
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if goos != "windows" {
		return false
	}
	for _, errno := range windowsClientGoneErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// Windows error codes for a peer that closed its end. They are spelled out
// as numbers so that clientGoneOn builds, and can be tested, on every
// system; syscall does not define ERROR_NO_DATA at all.
const (
	windowsErrorBrokenPipe = syscall.Errno(109)   // ERROR_BROKEN_PIPE
	windowsErrorNoData     = syscall.Errno(232)   // ERROR_NO_DATA
	windowsWSAEConnAborted = syscall.Errno(10053) // WSAECONNABORTED
	windowsWSAEConnReset   = syscall.Errno(10054) // WSAECONNRESET
)

// windowsClientGoneErrnos are the Windows error codes clientGoneOn treats
// as the peer having gone away: a reset or aborted socket connection, a
// pipe whose other end was closed (ERROR_BROKEN_PIPE) and a write to a
// pipe that is being closed (ERROR_NO_DATA).
var windowsClientGoneErrnos = []syscall.Errno{
	windowsErrorBrokenPipe,
	windowsErrorNoData,
	windowsWSAEConnAborted,
	windowsWSAEConnReset,
}

// waitDelay is how long after the context is done git may take to exit
// once its input was closed, before it is killed.
const waitDelay = 5 * time.Second

// gitPipeExitCode is the status git exits with when a write fails with a
// broken pipe and SIGPIPE did not end it (check_pipe: 128 + SIGPIPE), as on
// Windows, which has no SIGPIPE.
const gitPipeExitCode = 141

// brokenPipeExit reports whether err, returned by Wait for git, means git
// died of a broken pipe: killed by SIGPIPE on Unix, or exited with
// gitPipeExitCode.
func brokenPipeExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGPIPE {
		return true
	}
	return exitErr.ExitCode() == gitPipeExitCode
}

// outputClosedExit reports whether err, returned by Wait for git, with
// lastStderr the last line git wrote to stderr, means git died because its
// output was closed: of a broken pipe (brokenPipeExit), or with die()'s
// status 128 after a failed write to its output.
//
// The second case is Windows: git for Windows writes most output through
// stdio, where a write to a closed pipe fails with EINVAL rather than EPIPE,
// so maybe_flush_or_die and fflush_or_die report "fwrite error: Invalid
// argument" or "fflush error: Invalid argument" (or write_or_die "write
// error") and exit 128 instead of 141. die() exits 128 for every failure,
// so only the message tells them apart, and only the messages of a failed
// write to git's own output count: the message, after its "fatal: "
// prefix, must start with one of outputWriteFailures, or end with
// strerror's "Broken pipe". A failed write elsewhere, such as csum-file's
// "sha1 file '...' write error. Out of diskspace" or pkt-line's "packet
// write failed - data exceeds max packet size", stays a failure.
//
// The trade-offs: a death the client caused with another message (a failed
// packet_flush, or EOF on a read) is still logged as a failure. And git
// translates "packet write failed" and its siblings, and strerror's text
// follows the locale too (LANG and LC_* reach git through serviceEnv, and
// are not overridden, as hooks inherit them); on a localized server those
// deaths are logged as failures again. Only the write_or_die and
// maybe_flush_or_die messages ("fwrite error", "fflush error", "write
// error", "write failure on") are not translated.
func outputClosedExit(err error, lastStderr string) bool {
	if brokenPipeExit(err) {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		return false
	}
	msg := strings.TrimSpace(lastStderr)
	// Drop die()'s "fatal: ", whose text is translated, at the first ": ".
	if _, rest, ok := strings.Cut(msg, ": "); ok {
		msg = rest
	}
	msg = strings.ToLower(msg)
	if strings.HasSuffix(msg, "broken pipe") {
		return true
	}
	for _, m := range outputWriteFailures {
		if strings.HasPrefix(msg, m) {
			return true
		}
	}
	return false
}

// outputWriteFailures start the messages, lowercased and without die()'s
// prefix, git dies with when a write to its stdout or stderr fails:
// maybe_flush_or_die's "fwrite error" and "write failure on 'standard
// output'", fflush_or_die's "fflush error" (after the ref advertisement,
// and in protocol v2 ls-refs), write_or_die's "write error", and pkt-line's
// "packet write failed: ..." and "packet write with format failed".
var outputWriteFailures = []string{
	"fwrite error",
	"fflush error",
	"write error",
	"write failure on",
	"packet write failed:",
	"packet write with format failed",
}

// stoppedByContext reports whether err, returned by Wait for a command run
// with ctx, comes from the command being stopped because ctx was done rather
// than from the command failing on its own. cancelled reports that the
// command's Cancel ran: git was then told its input ended, and fails or is
// killed for it. Wait returns the context error itself when the command
// exits successfully after it was told to stop.
func stoppedByContext(ctx context.Context, err error, cancelled bool) bool {
	ctxErr := ctx.Err()
	return ctxErr != nil && (cancelled || errors.Is(err, ctxErr))
}

// ServiceCommand is used to run a git service command.
type ServiceCommand struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string
	Env    []string
	Args   []string
	// Config holds extra "key=value" git settings, each passed to git
	// with -c before the service name.
	Config []string

	// JoinStdin makes the handler wait, once git has exited, for the copy
	// of Stdin to git to end even when Stdin takes no read deadline that
	// could end it. Set it when Stdin must not be read after the handler
	// returns, as for an HTTP request body; the copy then ends when the
	// client finishes sending or goes away. See joinStdin.
	JoinStdin bool

	// Modifier functions
	CmdFunc func(*exec.Cmd)
}
