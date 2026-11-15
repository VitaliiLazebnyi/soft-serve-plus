package daemon

import (
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
