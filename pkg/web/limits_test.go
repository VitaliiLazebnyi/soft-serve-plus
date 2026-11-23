package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/internal/testdial"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// newLimitsServer serves the production router, with all its middleware,
// built with the HTTP limits configure sets, over a real connection, so
// deadlines apply. The limits rely on http.ResponseController reaching the
// connection through every middleware's response writer.
func newLimitsServer(t *testing.T, configure func(*config.HTTPConfig)) string {
	t.Helper()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be
	if _, err := be.CreateRepository(ctx, "repo", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	configure(&env.cfg.HTTP)
	srv := httptest.NewUnstartedServer(env.router(t))
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// closeWait is how long a test waits for the server to cut off a connection
// once a short timeout has passed. It is far past those timeouts, so a
// loaded machine that stalls the server for seconds does not fail the test,
// and the test does not time the cut-off: a connection still open after
// closeWait is one the timeout did not apply to.
const closeWait = time.Minute

// dial opens a connection to addr that fails the test if it is still open
// after timeout.
func dial(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// sendStalled sends a POST to the upload-pack route that announces a 1000
// byte body, sends a few bytes of it, and then sends nothing more.
func sendStalled(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := io.WriteString(conn, "POST /repo.git/git-upload-pack HTTP/1.1\r\n"+
		"Host: localhost\r\n"+
		"Content-Type: application/x-git-upload-pack-request\r\n"+
		"Content-Length: 1000\r\n"+
		"\r\n"+
		"0014command=ls-r"); err != nil {
		t.Fatal(err)
	}
}

// A client that stops sending its body is cut off after the idle timeout,
// which ends the git process serving it, rather than holding it forever.
func TestHTTPStalledBodyTimesOut(t *testing.T) {
	t.Parallel()
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 1
	})

	conn := dial(t, addr, closeWait)
	sendStalled(t, conn)
	start := time.Now()
	if _, err := io.Copy(io.Discard, conn); err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			t.Fatalf("connection still open after %s; the stalled request was not cut off", time.Since(start))
		}
	}
}

// A request is cut off after max_timeout even while the client keeps it
// alive.
func TestHTTPMaxTimeout(t *testing.T) {
	t.Parallel()
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 0
		c.MaxTimeout = 1
	})

	conn := dial(t, addr, closeWait)
	sendStalled(t, conn)
	start := time.Now()
	if _, err := io.Copy(io.Discard, conn); err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			t.Fatalf("connection still open after %s; max_timeout did not apply", time.Since(start))
		}
	}
}

// Requests over max_connections are turned away while the slots are taken,
// and served again once one is free.
func TestHTTPMaxConnections(t *testing.T) {
	t.Parallel()
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 0
		c.MaxConnections = 1
	})

	// Probe over a kept-alive connection: opening a connection per probe
	// makes macOS drop loopback connects now and then, which takes seconds.
	client := &http.Client{Transport: &http.Transport{}, Timeout: closeWait}
	t.Cleanup(client.CloseIdleConnections)
	status := func() int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"http://"+addr+"/repo.git/info/refs?service=git-upload-pack", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	if got := status(); got != http.StatusOK {
		t.Fatalf("status before the slot is taken = %d, want %d", got, http.StatusOK)
	}

	// The handler of the last probe may still hold the slot when the
	// stalled request arrives, which then gets the 503 itself. Send it
	// again until it is the one holding the slot.
	stalled := dial(t, addr, 2*closeWait)
	sendStalled(t, stalled)
	waitFor(t, func() bool {
		if status() == http.StatusServiceUnavailable {
			return true
		}
		if answered(t, stalled) {
			_ = stalled.Close()
			stalled = dial(t, addr, 2*closeWait)
			sendStalled(t, stalled)
		}
		return false
	}, "a request over max_connections was not turned away")

	// A request over the limit that stalls its body is answered at once,
	// rather than once the server has read the body it announced.
	over := dial(t, addr, closeWait)
	sendStalled(t, over)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(over), req)
	if err != nil {
		t.Fatalf("reading the answer to a stalled request over the limit: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stalled request over the limit: status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	_ = stalled.Close()
	waitFor(t, func() bool { return status() == http.StatusOK },
		"the slot was not freed once the stalled request ended")
}

// answered reports whether the server has answered or closed conn, which
// must otherwise stay open with nothing to read.
func answered(t *testing.T, conn net.Conn) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil { // a poll; waitFor retries for a minute
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		if err := conn.SetReadDeadline(time.Now().Add(2 * closeWait)); err != nil {
			t.Fatal(err)
		}
		return false
	}
	return true
}

// waitFor polls cond until it holds, failing the test with msg after
// closeWait: the conditions waited for need the server to see a close or
// end a request, which a loaded machine can delay for seconds.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(closeWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveLimits serves h within the deadlines l over a real connection.
func serveLimits(t *testing.T, l *requestDeadlines, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.serve(w, r, h)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// workIdle is the idle timeout of the tests that check that the server's
// work between reads and writes is not limited by it, and workTime the work
// they do, long enough that an idle deadline left armed would pass during
// it. A read or write in the tests moves a few bytes over loopback: the
// client has sent them before the handler reads (see sentBody), so only the
// server stalling for workIdle on bytes it already has could fail the test.
const (
	workIdle = 5 * time.Second
	workTime = workIdle * 3 / 2
)

// sentBody is a request body that closes sent once the client has taken all
// of it to send. A handler waits for that before it reads, so a client slow
// to send its body cannot make a read wait out the idle timeout: the test's
// subject is the server's work, not the client's.
type sentBody struct {
	r    *strings.Reader
	sent chan struct{}
	once sync.Once
}

func newSentBody(s string) *sentBody {
	return &sentBody{r: strings.NewReader(s), sent: make(chan struct{})}
}

func (b *sentBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.r.Len() == 0 {
		b.once.Do(func() { close(b.sent) })
	}
	return n, err
}

// newSentRequest returns a POST of body to url, whose length is known, so
// it is sent as it would be from a strings.Reader.
func newSentRequest(t *testing.T, url string, body *sentBody) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(body.r.Len())
	return req
}

// Once the body is read, time the server spends working before it answers
// is not limited by the idle timeout: the request is neither cancelled nor
// cut off.
func TestRequestLimitsIdleIgnoresServerWork(t *testing.T) {
	t.Parallel()
	l := &requestDeadlines{idle: workIdle}
	errc := make(chan error, 1)
	bodies := make(chan *sentBody, 1)
	url := serveLimits(t, l, func(w http.ResponseWriter, r *http.Request) {
		<-(<-bodies).sent
		if _, err := io.ReadAll(r.Body); err != nil {
			errc <- err
			return
		}
		select {
		case <-time.After(workTime):
		case <-r.Context().Done():
			errc <- r.Context().Err()
			return
		}
		errc <- nil
		_, _ = io.WriteString(w, "done")
	})

	body1 := newSentBody("body")
	bodies <- body1
	req := newSentRequest(t, url, body1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("handler: %v", err)
	}
	if string(body) != "done" {
		t.Fatalf("body = %q, want %q", body, "done")
	}

	// The connection is reused for a request without the limits' deadlines.
	body2 := newSentBody("again")
	bodies <- body2
	req = newSentRequest(t, url, body2)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close() //nolint:errcheck
	if _, err := io.ReadAll(resp2.Body); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("second request: %v", err)
	}
}

// Time the server spends working between reads of the body and between
// writes of the response is not limited by the idle timeout, over HTTP/1 and
// HTTP/2 alike. HTTP/2 resets a stream when a deadline passes even with no
// read or write in progress, so a deadline must not stay armed between them.
func TestRequestLimitsIdleIgnoresWorkBetweenReadsAndWrites(t *testing.T) {
	t.Parallel()
	for _, h2 := range []bool{false, true} {
		name := "HTTP1"
		if h2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := &requestDeadlines{idle: workIdle}
			errc := make(chan error, 1)
			reqBody := newSentBody("body")
			work := func(r *http.Request) error {
				select {
				case <-time.After(workTime):
					return nil
				case <-r.Context().Done():
					return r.Context().Err()
				}
			}
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				l.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					errc <- func() error {
						<-reqBody.sent
						if _, err := io.ReadFull(r.Body, make([]byte, 1)); err != nil {
							return err
						}
						if err := work(r); err != nil {
							return err
						}
						if _, err := io.ReadAll(r.Body); err != nil {
							return err
						}
						if _, err := io.WriteString(w, "a"); err != nil {
							return err
						}
						w.(http.Flusher).Flush()
						if err := work(r); err != nil {
							return err
						}
						_, err := io.WriteString(w, "b")
						return err
					}()
				}))
			}))
			if h2 {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			t.Cleanup(srv.Close)

			resp, err := srv.Client().Do(newSentRequest(t, srv.URL, reqBody))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close() //nolint:errcheck
			if want := map[bool]int{false: 1, true: 2}[h2]; resp.ProtoMajor != want {
				t.Fatalf("protocol = %s, want HTTP/%d", resp.Proto, want)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the response: %v", err)
			}
			if err := <-errc; err != nil {
				t.Fatalf("handler: %v", err)
			}
			if string(body) != "ab" {
				t.Fatalf("body = %q, want %q", body, "ab")
			}
		})
	}
}

// Over HTTP/2 too, a client that stops reading the response is cut off
// after the idle timeout.
//
// The connection is an in-memory pipe: the test is about the HTTP/2 stream,
// not the socket, and a pipe has no loopback connect for a loaded macOS
// machine to stall or fail.
func TestRequestLimitsIdleStalledReaderHTTP2(t *testing.T) {
	t.Parallel()
	l := &requestDeadlines{idle: 200 * time.Millisecond}
	// protocol is the request's protocol as the handler saw it, set before
	// started is closed.
	var protocol string
	started := make(chan struct{})
	errc := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol = r.Proto
		close(started)
		l.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 32*1024)
			for {
				if _, err := w.Write(buf); err != nil {
					errc <- err
					return
				}
				w.(http.Flusher).Flush()
			}
		}))
	}))
	n := &testdial.Network{}
	_ = srv.Listener.Close()
	srv.Listener = n.Listen()
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Transport.(*http.Transport).DialContext = n.Dial

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// On a loaded machine the server may reset the stream before the
	// client has read the response header; the write was cut off all the
	// same. But a request that failed before it reached the handler says
	// nothing about the cut-off, so the handler must have started. The
	// handler checks the protocol, as it sees it whether or not the client
	// gets the response header.
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	select {
	case <-started:
	case <-time.After(closeWait):
		t.Fatalf("the request did not reach the handler: %v", err)
	}
	if protocol != "HTTP/2.0" {
		t.Fatalf("protocol = %s, want HTTP/2.0", protocol)
	}
	select {
	case <-errc: //nolint:errcheck // only the write ending matters, not its error
	case <-time.After(closeWait):
		t.Fatal("writing to a client that does not read was not cut off")
	}
}

// releasedBody is a request body whose Read blocks until release is closed.
type releasedBody struct {
	io.ReadCloser
	release <-chan struct{}
	done    chan<- struct{}
}

func (b *releasedBody) Read([]byte) (int, error) {
	<-b.release
	defer close(b.done)
	return 0, io.EOF
}

// A read of the body that returns after the handler has returned, as git's
// stdin copy may, must not touch the connection's deadlines: over HTTP/2 the
// stream's state is gone by then, and setting a deadline panics outside the
// handler, killing the server.
func TestRequestLimitsReadAfterHandlerHTTP2(t *testing.T) {
	t.Parallel()
	l := &requestDeadlines{idle: time.Minute}
	release := make(chan struct{})
	readDone := make(chan struct{})
	reading := make(chan struct{})
	readReturned := make(chan struct{})
	addrs := make(chan string, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addrs <- r.RemoteAddr
		if r.URL.Path == "/after" {
			// The connection runs one handler at a time, so this one
			// starts only once the first one has returned and the
			// server has dropped its stream's state: let the read
			// return now.
			close(release)
			return
		}
		r.Body = &releasedBody{ReadCloser: r.Body, release: release, done: readDone}
		l.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			go func() {
				close(reading)
				_, _ = r.Body.Read(make([]byte, 1))
				// A deadline set by the read panics, and kills the
				// test binary, before this.
				close(readReturned)
			}()
			<-reading
			_, _ = io.WriteString(w, "ok")
		}))
	}))
	srv.EnableHTTP2 = true
	// One stream, and so one handler, at a time on the connection: the
	// server starts the second request's handler only after the first
	// one's has returned and its response writer has let go of the
	// stream.
	srv.Config.HTTP2 = &http.HTTP2Config{MaxConcurrentStreams: 1}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := srv.Client()
	// The second request waits for the first stream to close instead of
	// opening a connection of its own.
	client.Transport.(*http.Transport).HTTP2 = &http.HTTP2Config{StrictMaxConcurrentRequests: true}

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol = %s, want HTTP/2", resp.Proto)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	afterReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/after", nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := client.Do(afterReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = after.Body.Close()
	if first, second := <-addrs, <-addrs; first != second {
		t.Fatalf("the second request came over %s, not over the first one's connection %s", second, first)
	}
	select {
	case <-readDone:
	case <-time.After(closeWait):
		t.Fatal("the read did not return")
	}
	select {
	case <-readReturned:
	case <-time.After(closeWait):
		t.Fatal("the read did not return to the goroutine that made it")
	}
}

// A client that stops reading the response is cut off after the idle
// timeout, and the request is cancelled.
func TestRequestLimitsIdleStalledReader(t *testing.T) {
	t.Parallel()
	l := &requestDeadlines{idle: 200 * time.Millisecond}
	errc := make(chan error, 1)
	url := serveLimits(t, l, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32*1024)
		for {
			if _, err := w.Write(buf); err != nil {
				<-r.Context().Done()
				errc <- err
				return
			}
		}
	})

	conn := dial(t, strings.TrimPrefix(url, "http://"), 2*closeWait)
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Fatalf("write error = %v, want a timeout", err)
		}
	case <-time.After(closeWait):
		t.Fatal("writing to a client that does not read was not cut off")
	}
}

func TestDeadlinesNext(t *testing.T) {
	t.Parallel()
	d := &deadlines{}
	if got := d.next(); !got.IsZero() {
		t.Fatalf("next without limits = %v, want zero", got)
	}

	limit := time.Now().Add(time.Hour)
	d = &deadlines{max: limit}
	if got := d.next(); !got.Equal(limit) {
		t.Fatalf("next with only max = %v, want %v", got, limit)
	}

	d = &deadlines{idle: time.Minute, max: limit}
	if got := d.next(); !got.After(time.Now()) || !got.Before(limit) {
		t.Fatalf("next = %v, want about a minute from now", got)
	}

	d = &deadlines{idle: 2 * time.Hour, max: limit}
	if got := d.next(); !got.Equal(limit) {
		t.Fatalf("next past max = %v, want %v", got, limit)
	}
}

// A request that announces a body and never sends it is cut off after the
// idle timeout on every route, not only on the git routes: net/http reads
// the body a handler left unread before it answers, and must not wait for
// it with no deadline.
func TestHTTPStalledBodyOutsideGitRoutes(t *testing.T) {
	t.Parallel()
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 1
		c.MaxTimeout = 0
	})

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/nothing-here"},            // not found
		{http.MethodPut, "/repo.git/info/refs"},       // method not allowed
		{http.MethodPut, "/repo.git/git-upload-pack"}, // method not allowed on a git route
		{http.MethodGet, "/livez"},
		{http.MethodGet, "/repo?go-get=1"},
		{http.MethodOptions, "/repo.git/info/refs"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			conn := dial(t, addr, closeWait)
			if _, err := io.WriteString(conn, tc.method+" "+tc.path+" HTTP/1.1\r\n"+
				"Host: localhost\r\n"+
				"Content-Length: 100\r\n"+
				"\r\n"); err != nil {
				t.Fatal(err)
			}
			assertClosed(t, conn)
		})
	}
}

// assertClosed reads conn until the server closes it, and fails the test if
// conn's deadline, set by dial to closeWait, passes first.
func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	start := time.Now()
	_, err := io.Copy(io.Discard, conn)
	// A reset instead of an orderly close is fine: the server may close
	// with the announced body unread.
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatalf("connection still open after %s; the stalled request was not cut off", time.Since(start))
	}
}

// A handler that writes more than the response buffer before reading the
// body makes the HTTP/1 server read and discard the body on that write, not
// after the handler returns. The read deadline must bound that too, or a
// client that announces a body and never sends it holds the connection, and
// a git slot, for as long as it likes: an LFS download or a large ref
// advertisement writes this way.
func TestRequestDeadlinesStalledBodyDuringWrite(t *testing.T) {
	t.Parallel()
	for _, flush := range []bool{false, true} {
		name := "large write"
		if flush {
			name = "flush"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := &requestDeadlines{idle: 500 * time.Millisecond}
			url := serveLimits(t, l, func(w http.ResponseWriter, _ *http.Request) {
				if flush {
					_, _ = io.WriteString(w, "x")
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(make([]byte, 8<<10))
			})
			conn := dial(t, strings.TrimPrefix(url, "http://"), closeWait)
			if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\n"+
				"Host: localhost\r\n"+
				"Content-Length: 100\r\n"+
				"\r\n"); err != nil {
				t.Fatal(err)
			}
			assertClosed(t, conn)
		})
	}
}

// One client address cannot take every git slot: once it holds its share,
// its next request is turned away while another address is still served,
// and it is served again once one of its requests ends.
func TestHTTPMaxConnectionsPerIP(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be
	if _, err := be.CreateRepository(ctx, "repo", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := &env.cfg.HTTP
	cfg.IdleTimeout = 0
	cfg.MaxConnections = 8
	cfg.MaxConnectionsPerIP = 2
	h := env.router(t)

	const a, b = "192.0.2.1:1000", "198.51.100.7:1000"
	status := func(addr string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/repo.git/info/refs?service=git-upload-pack", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	// held is an upload-pack request whose body stays open until body is
	// closed, so it keeps its slot; done is closed once it is answered.
	type held struct {
		body *io.PipeWriter
		done chan struct{}
	}
	var wg sync.WaitGroup
	hold := func(addr string) held {
		pr, pw := io.Pipe()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/repo.git/git-upload-pack", pr)
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		req.RemoteAddr = addr
		hr := held{body: pw, done: make(chan struct{})}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(hr.done)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
		return hr
	}
	holds := []held{hold(a), hold(a)}
	t.Cleanup(func() {
		for _, hr := range holds {
			_ = hr.body.Close()
		}
		wg.Wait()
	})

	// A probe may take a slot just before a held request asks for one,
	// which is then turned away itself. Send such a request again until
	// both are the ones holding the slots.
	waitFor(t, func() bool {
		if status(a) == http.StatusServiceUnavailable {
			return true
		}
		for i, hr := range holds {
			select {
			case <-hr.done:
				_ = hr.body.Close()
				holds[i] = hold(a)
			default:
			}
		}
		return false
	}, "an address holding its share of the slots was not turned away")
	// Another port of the same address shares its share.
	if got := status("192.0.2.1:2000"); got != http.StatusServiceUnavailable {
		t.Fatalf("same address, other port: status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := status(b); got != http.StatusOK {
		t.Fatalf("other address: status = %d, want %d", got, http.StatusOK)
	}

	_ = holds[0].body.Close()
	waitFor(t, func() bool { return status(a) == http.StatusOK },
		"the address was not served again once one of its requests ended")
}

// Requests are counted per IPv4 address and per IPv6 /64, except from
// loopback, where a reverse proxy on the same host connects from.
func TestClientPrefix(t *testing.T) {
	t.Parallel()
	key := func(addr string) string {
		p, ok := clientPrefix(addr)
		if !ok {
			return ""
		}
		return p.String()
	}
	for addr, want := range map[string]string{
		"192.0.2.1:1000":          "192.0.2.1/32",
		"[::ffff:192.0.2.1]:1000": "192.0.2.1/32",
		"[2001:db8::1]:1000":      "2001:db8::/64",
		"[2001:db8::ffff:1]:2000": "2001:db8::/64",
		"[2001:db8:0:1::1]:1000":  "2001:db8:0:1::/64",
		"[fe80::1%eth0]:1000":     "fe80::/64",
		"127.0.0.1:1000":          "",
		"127.1.2.3:1000":          "",
		"[::1]:1000":              "",
		"[::ffff:127.0.0.1]:1000": "",
		"pipe":                    "",
		"":                        "",
	} {
		if got := key(addr); got != want {
			t.Errorf("clientPrefix(%q) = %q, want %q", addr, got, want)
		}
	}

	// The zero value limits nothing; an address over its share takes no
	// slot of the total.
	var none requestLimits
	for range 100 {
		if _, ok := none.acquire("192.0.2.1:1"); !ok {
			t.Fatal("the zero value refused a request")
		}
	}
	l := &requestLimits{max: 3, maxPerAddr: 1}
	release, ok := l.acquire("192.0.2.1:1")
	if !ok {
		t.Fatal("first request refused")
	}
	if _, ok := l.acquire("192.0.2.1:2"); ok {
		t.Fatal("second request from the address was let through")
	}
	for _, addr := range []string{"192.0.2.2:1", "pipe"} {
		if _, ok := l.acquire(addr); !ok {
			t.Fatalf("request from %q refused", addr)
		}
	}
	if _, ok := l.acquire("192.0.2.3:1"); ok {
		t.Fatal("a request over the total was let through")
	}
	release()
	if _, ok := l.acquire("192.0.2.1:3"); !ok {
		t.Fatal("the address was refused after its request ended")
	}

	// Loopback peers have no share of their own: they are bound only by
	// the total.
	l = &requestLimits{max: 4, maxPerAddr: 1}
	for i := range 4 {
		if _, ok := l.acquire("127.0.0.1:" + strconv.Itoa(1000+i)); !ok {
			t.Fatalf("loopback request %d refused under the total", i+1)
		}
	}
	if _, ok := l.acquire("[::1]:1000"); ok {
		t.Fatal("a loopback request over the total was let through")
	}
}
