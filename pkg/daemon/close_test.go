package daemon

import (
	"context"
	"net"
	"testing"
	"time"
)

// startCloseTestDaemon serves a daemon like the test daemon whose request
// and idle timeouts are ten minutes, far past the tests' one-minute wait,
// and which has no total time limit on a connection, so a request that ends
// within the wait can only have been ended by Close. opts change the daemon
// before it serves.
func startCloseTestDaemon(t *testing.T, opts ...func(*GitDaemon)) (*GitDaemon, string) {
	t.Helper()
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 600
	cfg.Git.MaxTimeout = 0
	d, err := NewGitDaemon(&cfg, testBackend, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.requestTimeout = 10 * time.Minute
	for _, opt := range opts {
		opt(d)
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })
	return d, l.Addr().String()
}

// waitHandlers waits up to idleWait for every goroutine of d, its accept
// loop and its handlers, to return.
func waitHandlers(t *testing.T, d *GitDaemon) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(idleWait):
		t.Fatal("a handler is still running after Close")
	}
}

// Close ends a request the daemon is still reading, and its handler
// returns, rather than waiting out the ten-minute request timeout: the
// daemon has no context of its own, so closing the connection is what ends
// it.
func TestCloseEndsRequestRead(t *testing.T) {
	d, addr := startCloseTestDaemon(t)
	dialDaemonAt(t, addr)
	// The daemon is reading the request once it counts the connection.
	waitConns(t, d, 1, idleWait)
	_ = d.Close()
	waitHandlers(t, d)
}

// Close ends the context of a request being served, so the git command and
// the database lookups it runs stop, and not only the connection's reads
// and writes.
func TestCloseEndsRequestContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	d, addr := startCloseTestDaemon(t, func(d *GitDaemon) {
		d.testHooks.beforeAuthorize = func(ctx context.Context) {
			close(started)
			<-release
			ctxErr <- ctx.Err()
		}
	})
	c := dialDaemonAt(t, addr)
	if _, err := c.Write([]byte(pkt("git-upload-pack /close-public.git\x00host=localhost\x00"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(idleWait):
		t.Fatal("the request was not served")
	}
	// The hook holds the handler while the daemon closes; it then reads
	// the context it was served with.
	_ = d.Close()
	close(release)
	if err := <-ctxErr; err == nil {
		t.Error("the request's context was not done after Close")
	}
	waitHandlers(t, d)
}
