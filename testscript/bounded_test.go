package testscript

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/crypto/ssh"
)

// commandTimeout bounds how long a git command, or a soft or ui command
// over SSH, may run. Each finishes in seconds even on a loaded machine; one
// still running after commandTimeout is stuck, and without a bound it would
// hang its script until the test binary times out, which can be an hour,
// and then fail with a message that says nothing about where it was stuck.
// It is not a budget for the command: it only turns a hang into a failure
// that names the command and shows its processes and the server's
// goroutines. It is a variable only so that TestCommandTimeout can lower
// it; nothing else changes it.
var commandTimeout = 5 * time.Minute

// boundedEnv, in the environment of the test binary, makes it run its
// arguments as a command bounded by the duration it holds (see runBounded)
// instead of running the tests.
const boundedEnv = "SOFT_SERVE_TEST_BOUNDED"

// boundedTimeoutMarker starts the line runBounded writes to stderr when its
// command did not exit in time.
const boundedTimeoutMarker = "harness: command did not exit within"

// stuckMarker starts the message a script fails with when one of its
// commands did not finish within commandTimeout (see failStuck).
const stuckMarker = "harness: command stuck:"

// connectAttempts is how many times execBounded runs a git command whose
// connection to the server failed (see connectFailed).
const connectAttempts = 3

// connectFailed matches what git prints when its TCP connect to the HTTP
// server or the git daemon fails: curl's "Failed to connect to 127.0.0.1
// port N after N ms: Couldn't connect to server", and, for the daemon,
// git's own "unable to connect to 127.0.0.1:" on a line of its own. Only
// git's own fatal line counts, not a message from the server ("remote:
// fatal: ...") or a later error that quotes curl. Under load a loopback
// connect can time out (KNOWN_ISSUES item 309), at about 7.8 s on macOS.
// Such a request never reached the server, so running the command again
// repeats nothing the server saw. git's ssh retries its own connect
// (ConnectionAttempts in sshConfig).
var connectFailed = regexp.MustCompile(`(?m)^fatal: (unable to access '[^']*': Failed to connect to \S+ port \d+|unable to connect to \S+:$)`)

// execBounded runs name with args as ts.Exec does, through the test binary
// in runBounded, which kills the command if it has not exited within
// commandTimeout. A command killed so fails the script, also under "!",
// where any failure would otherwise pass. A git command whose connect to
// the server failed is run again, up to connectAttempts times in all.
func execBounded(ts *testscript.TestScript, name string, args ...string) error {
	self, err := os.Executable()
	ts.Check(err)
	for attempt := 1; ; attempt++ {
		ts.Setenv(boundedEnv, commandTimeout.String())
		err = ts.Exec(self, append([]string{name}, args...)...)
		ts.Setenv(boundedEnv, "")
		stderr := ts.ReadFile("stderr")
		if err != nil && strings.Contains(stderr, boundedTimeoutMarker) {
			failStuck(ts, name+" "+strings.Join(args, " "))
		}
		if err == nil || attempt == connectAttempts || !connectFailed.MatchString(stderr) {
			return err
		}
		ts.Logf("harness: %s could not connect to the server (attempt %d of %d); running it again", name, attempt, connectAttempts)
	}
}

// failStuck fails the script for command, which did not finish within
// commandTimeout. It first has each soft serve the script started print its
// goroutines (see quitProcess), which testscript logs with the script's
// failure, so the log shows whether the server was still serving the
// command.
func failStuck(ts *testscript.TestScript, command string) {
	for _, c := range ts.BackgroundCmds() {
		if len(c.Args) > 1 && c.Args[1] == "serve" && c.Process != nil {
			if err := quitProcess(c.Process); err != nil {
				ts.Logf("cannot have soft serve (pid %d) print its goroutines: %v", c.Process.Pid, err)
			}
		}
	}
	ts.Fatalf("%s %q did not finish within %v and was stopped; its processes are listed above, and the server's goroutines below", stuckMarker, command, commandTimeout)
}

// runBounded runs args, with the test binary's standard input and output,
// and returns its exit code. If it has not exited within limit, runBounded
// writes the processes it started, and their open files, to stderr, kills
// them all and returns 124.
//
// testscript interrupts it with SIGQUIT when the test binary's deadline
// comes, and kills it a grace period later. runBounded then lists the
// processes with ps only, which is quick, and kills them before that grace
// period is over, so none is left running. On Windows testscript kills it
// at once instead (os.Kill), so git and its ssh are left running, as they
// were before runBounded.
func runBounded(limit string, args []string) int {
	d, err := time.ParseDuration(limit)
	if err != nil || len(args) == 0 {
		fmt.Fprintf(os.Stderr, "harness: bad bounded command %q %q: %v\n", limit, args, err)
		return 2
	}
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = withoutEnv(os.Environ(), boundedEnv)
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, interruptSignals...)
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case err := <-done:
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit) && exit.ExitCode() >= 0:
			return exit.ExitCode()
		default:
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case <-timer.C:
		fmt.Fprintf(os.Stderr, "%s %v: %s\n", boundedTimeoutMarker, d, strings.Join(args, " "))
		describeProcesses(os.Stderr, cmd.Process.Pid, true)
	case s := <-sigs:
		fmt.Fprintf(os.Stderr, "harness: %v while running %s\n", s, strings.Join(args, " "))
		describeProcesses(os.Stderr, cmd.Process.Pid, false)
	}
	killProcessTree(cmd.Process)
	<-done //nolint:errcheck // killed: its exit status says nothing
	return 124
}

// withoutEnv returns env without the entries for key.
func withoutEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return out
}

// diagnosticTimeout bounds each command runDiagnostic runs, so that a slow
// lsof on a loaded machine does not hold up killing the stuck command.
const diagnosticTimeout = 10 * time.Second

// runDiagnostic runs a command that describes processes, for
// diagnosticTimeout at most, and returns its combined output, or why it
// could not run.
func runDiagnostic(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s%s %s: %v\n", out, name, strings.Join(args, " "), err)
	}
	return string(out)
}

// sshBound is the bound dialSSH puts on a connection.
type sshBound struct {
	timer *time.Timer
	hit   atomic.Bool
}

// fired reports whether the connection was closed for running longer than
// commandTimeout.
func (b *sshBound) fired() bool { return b.hit.Load() }

// stop ends the bound, once the command has finished.
func (b *sshBound) stop() { b.timer.Stop() }

// dialSSH connects to the script's SSH server as config says. The
// connection is closed commandTimeout after it was opened, which ends
// whatever is still using it; the caller calls stop once its command has
// finished and then fired to find out whether that happened. A connection
// or handshake that has not finished in that time fails the script; any
// other failure is returned.
func dialSSH(ts *testscript.TestScript, config *ssh.ClientConfig) (*ssh.Client, *sshBound, error) {
	addr := net.JoinHostPort(loopback, ts.Getenv("SSH_PORT"))
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if errors.Is(err, context.DeadlineExceeded) {
		failStuck(ts, "connect to "+addr)
	}
	if err != nil {
		return nil, nil, err
	}

	b := &sshBound{}
	b.timer = time.AfterFunc(commandTimeout, func() {
		b.hit.Store(true)
		_ = conn.Close()
	})
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		b.stop()
		_ = conn.Close()
		if b.fired() {
			failStuck(ts, "SSH handshake with "+addr)
		}
		return nil, nil, err
	}
	return ssh.NewClient(c, chans, reqs), b, nil
}
