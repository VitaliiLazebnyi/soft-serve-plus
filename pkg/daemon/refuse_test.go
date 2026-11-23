package daemon

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/gitserve"
)

// TestRefusalEndsAtRequestDeadline checks that a refusal after access checks
// that outlast the request timeout does not wait on the client past the
// request deadline. A wait counted again from the refusal would let slow
// checks push it past the deadline by as long as they took.
//
// Nothing here is decided by how long a step takes. The request timeout is
// an hour, so no stall makes the request line late, and the refusal reads
// its time from a clock two request timeouts ahead, so it comes after the
// request deadline however soon it runs. Past that deadline the daemon
// lets go of the connection at once; a wait counted from the refusal would
// hold it for about the request timeout more, an hour, far past the test's
// wait.
func TestRefusalEndsAtRequestDeadline(t *testing.T) {
	t.Parallel()
	const requestTimeout = time.Hour
	d, addr := startLimitedDaemon(t, 1, func(d *Server) {
		d.requestTimeout = requestTimeout
		d.refuseLinger = time.Hour
		d.testHooks.now = func() time.Time { return time.Now().Add(2 * requestTimeout) }
	})
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if _, err := io.WriteString(c, pkt("git-upload-pack /deadline-missing.git\x00host=localhost\x00")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, err = readPktline(c)
	wantErrLine(t, "missing repo", err, gitserve.ErrNotAuthed)
	// The client neither sends nor hangs up, so only the end of the
	// refusal's wait closes the connection.
	waitConns(t, d, 0, idleWait)
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
	d, addr := startLimitedDaemon(t, 1, func(d *Server) { d.refuseLinger = time.Hour })
	dialHeld(t, addr, pkt("git-receive-pack /repo.git\x00host=localhost\x00"), ErrPushNotSupported)
	// The refusal waits on the client, which neither sends nor hangs up.
	waitConns(t, d, 1, idleWait)
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	waitConns(t, d, 0, idleWait)
}

// holdSlot dials addr and waits until d counts the connection, which then
// holds one of d's slots until it is closed or the test ends.
func holdSlot(t *testing.T, d *Server, addr string) net.Conn {
	t.Helper()
	want := d.conns.Size() + 1
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitConns(t, d, want, idleWait)
	return c
}

// TestMaxConnectionsRefusalSurvivesLateInput checks that a client turned
// away for the connection limit can keep writing, as git archive writes its
// arguments, without the daemon resetting the connection under it: a
// connection closed at once would draw a reset with the client's next
// write, and a client still writing could lose the error to it.
func TestMaxConnectionsRefusalSurvivesLateInput(t *testing.T) {
	t.Parallel()
	conns := make(chan *recordedConn, 2)
	d, addr := startDaemonOn(t, 1, func(inner net.Listener) net.Listener {
		return &recordingListener{Listener: inner, conns: conns}
	}, func(d *Server) { d.refuseLinger = time.Hour })
	accepted := func() *recordedConn {
		t.Helper()
		select {
		case c := <-conns:
			return c
		case <-time.After(idleWait):
			t.Fatal("the daemon did not accept the connection")
			return nil
		}
	}
	holdSlot(t, d, addr)
	accepted() // the held one
	c := dialHeld(t, addr, "", gitserve.ErrMaxConnections)
	refused := accepted() // accepted before it was refused
	sent := 0
	for i := range 20 {
		n, err := io.WriteString(c, pkt("argument HEAD\n"))
		if err != nil {
			t.Fatalf("write %d after the refusal: %v", i+1, err)
		}
		sent += n
	}
	refused.waitRead(t, sent)
	// The daemon shut its side after the error and its flush packet: the
	// client reads a clean end of the stream, not a reset.
	if out, err := io.ReadAll(c); err != nil || (len(out) != 0 && string(out) != "0000") {
		t.Fatalf("after the refusal: read %q, %v; want a clean end", out, err)
	}
}

// TestMaxConnectionsRefusalsDoNotBlockAccept checks that refusals that wait
// on their clients do not hold up the accept loop, also past maxRefusing of
// them, and that Shutdown does not wait on them.
func TestMaxConnectionsRefusalsDoNotBlockAccept(t *testing.T) {
	t.Parallel()
	d, addr := startLimitedDaemon(t, 1, func(d *Server) { d.refuseLinger = time.Hour })
	held := holdSlot(t, d, addr)
	for range maxRefusing + 4 {
		dialHeld(t, addr, "", gitserve.ErrMaxConnections)
	}
	// The first maxRefusing refusals still wait on their clients; the
	// rest were closed at once.
	if n := len(d.refusing); n != maxRefusing {
		t.Errorf("%d refusals in progress, want %d", n, maxRefusing)
	}
	if got := d.conns.Size(); got != 1 {
		t.Errorf("%d connections counted, want 1", got)
	}
	// Shutdown would wait for the connection in the slot, which has not
	// sent its request yet.
	_ = held.Close()
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}
