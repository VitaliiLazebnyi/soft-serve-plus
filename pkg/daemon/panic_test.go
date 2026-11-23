package daemon

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
)

// TestHandlerPanicRecovered checks that a panic while a connection is
// handled is logged and does not end the daemon: the connection is closed,
// its slot and its place in the WaitGroup are given back, and the next
// client is served. The handler's goroutine had no recover, so such a panic
// ended the whole server, and its wg.Done was not deferred.
func TestHandlerPanicRecovered(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	var panicked atomic.Bool
	d, addr := startLimitedDaemon(t, 1, func(d *Server) {
		d.logger = log.New(logs)
		d.testHooks.beforeAuthorize = func(context.Context) {
			if panicked.CompareAndSwap(false, true) {
				panic("injected handler panic")
			}
		}
	})
	request := pkt("git-upload-pack /panic-missing.git\x00host=localhost\x00")

	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// The daemon closes the connection without an answer.
	if out, err := io.ReadAll(c); len(out) != 0 || (err != nil && !isReset(err)) {
		t.Fatalf("after the panic: read %q, %v; want the connection closed", out, err)
	}
	if !panicked.Load() {
		t.Fatal("the hook did not run")
	}
	// The slot is free again: with a limit of 1, the next client is
	// served rather than refused.
	waitConns(t, d, 0, idleWait)
	c2 := dialHeld(t, addr, request, gitserve.ErrNotAuthed)
	_ = c2.Close()

	// The panic is logged by safego.Run's recover, which runs after
	// handleClient's deferred close: the client can see the connection
	// closed, and the next client be served, before the line is written.
	waitLog(t, logs, "recovered panic", idleWait)
	waitLog(t, logs, "injected handler panic", idleWait)
	// The handler's goroutine is no longer counted: Shutdown does not
	// wait for it.
	waitConns(t, d, 0, idleWait)
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}
