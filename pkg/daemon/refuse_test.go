package daemon

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

// TestRefusalEndsAtRequestDeadline checks that a refusal after access checks
// that outlast the request timeout does not wait on the client past the
// request deadline. The wait used to be what was left of the request timeout
// when the request line arrived, counted again from the refusal, so slow
// checks pushed it past the deadline by as long as they took.
func TestRefusalEndsAtRequestDeadline(t *testing.T) {
	t.Parallel()
	const requestTimeout = 4 * time.Second
	release := make(chan time.Time, 1)
	d, addr := startLimitedDaemon(t, 1, func(d *GitDaemon) {
		d.requestTimeout = requestTimeout
		d.refuseLinger = time.Hour
		// Keep the access checks busy until past the request deadline.
		d.testHooks.beforeAuthorize = func() { time.Sleep(time.Until(<-release)) }
	})
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	// The daemon has accepted the connection once it counts it, so its
	// request deadline is before now plus the request timeout.
	waitConns(t, d, 1, 30*time.Second)
	release <- time.Now().Add(requestTimeout)
	if _, err := io.WriteString(c, pkt("git-upload-pack /deadline-missing.git\x00host=localhost\x00")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, err = readPktline(c)
	wantErrLine(t, "missing repo", err, git.ErrNotAuthed)
	// The request deadline has passed, so the daemon lets go of the
	// connection at once. The old wait would hold it for about the
	// request timeout more.
	waitConns(t, d, 0, requestTimeout/2)
}

// dialHeld connects to addr, sends request, and reads the daemon's answer,
// which must be the ERR packet want. The connection stays open, sending
// nothing more, until the test ends.
func dialHeld(t *testing.T, addr, request string, want error) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	if request != "" {
		if _, err := io.WriteString(c, request); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
	_, err = readPktline(c)
	wantErrLine(t, "answer", err, want)
	return c
}

// TestShutdownCutsRefusalWait checks that a graceful Shutdown does not wait
// for a refused client to hang up.
func TestShutdownCutsRefusalWait(t *testing.T) {
	t.Parallel()
	d, addr := startLimitedDaemon(t, 1, func(d *GitDaemon) { d.refuseLinger = time.Hour })
	dialHeld(t, addr, pkt("git-receive-pack /repo.git\x00host=localhost\x00"), ErrPushNotSupported)
	// The refusal waits on the client, which neither sends nor hangs up.
	waitConns(t, d, 1, 30*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	waitConns(t, d, 0, 30*time.Second)
}

// TestServerContextCutsRefusalWait checks that a refusal stops waiting on
// the client when the server's context ends.
func TestServerContextCutsRefusalWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d, addr := startLimitedDaemon(t, 1, func(d *GitDaemon) {
		d.refuseLinger = time.Hour
		d.ctx = ctx
	})
	dialHeld(t, addr, pkt("git-receive-pack /repo.git\x00host=localhost\x00"), ErrPushNotSupported)
	waitConns(t, d, 1, 30*time.Second)
	cancel()
	waitConns(t, d, 0, 15*time.Second)
}
