package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// newLimitsServer serves the production router, with all its middleware,
// built with the HTTP limits configure sets, over a real connection, so
// deadlines apply. The limits rely on http.ResponseController reaching the
// connection through every middleware's response writer.
func newLimitsServer(t *testing.T, configure func(*config.HTTPConfig)) string {
	t.Helper()
	ctx, be, _ := newLFSTestContext(t)
	if _, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	configure(&config.FromContext(ctx).HTTP)
	srv := httptest.NewUnstartedServer(NewRouter(ctx))
	srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

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
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 1
	})

	conn := dial(t, addr, 15*time.Second)
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
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 0
		c.MaxTimeout = 1
	})

	conn := dial(t, addr, 15*time.Second)
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
	addr := newLimitsServer(t, func(c *config.HTTPConfig) {
		c.IdleTimeout = 0
		c.MaxConnections = 1
	})

	// Probe over a kept-alive connection: opening a connection per probe
	// makes macOS drop loopback connects now and then, which takes seconds.
	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
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
	stalled := dial(t, addr, 30*time.Second)
	sendStalled(t, stalled)
	waitFor(t, func() bool {
		if status() == http.StatusServiceUnavailable {
			return true
		}
		if answered(t, stalled) {
			_ = stalled.Close()
			stalled = dial(t, addr, 30*time.Second)
			sendStalled(t, stalled)
		}
		return false
	}, "a request over max_connections was not turned away")

	// A request over the limit that stalls its body is answered at once,
	// rather than once the server has read the body it announced.
	over := dial(t, addr, 10*time.Second)
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
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return false
	}
	return true
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveLimits serves h within l over a real connection.
func serveLimits(t *testing.T, l *requestLimits, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.serve(w, r, h)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Once the body is read, time the server spends working before it answers
// is not limited by the idle timeout: the request is neither cancelled nor
// cut off.
func TestRequestLimitsIdleIgnoresServerWork(t *testing.T) {
	l := &requestLimits{idle: 200 * time.Millisecond}
	errc := make(chan error, 1)
	url := serveLimits(t, l, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			errc <- err
			return
		}
		select {
		case <-time.After(4 * l.idle):
		case <-r.Context().Done():
			errc <- r.Context().Err()
			return
		}
		errc <- nil
		_, _ = io.WriteString(w, "done")
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
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
	req, err = http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader("again"))
	if err != nil {
		t.Fatal(err)
	}
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
			l := &requestLimits{idle: 200 * time.Millisecond}
			errc := make(chan error, 1)
			work := func(r *http.Request) error {
				select {
				case <-time.After(4 * l.idle):
					return nil
				case <-r.Context().Done():
					return r.Context().Err()
				}
			}
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				l.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					errc <- func() error {
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

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
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
func TestRequestLimitsIdleStalledReaderHTTP2(t *testing.T) {
	t.Parallel()
	l := &requestLimits{idle: 200 * time.Millisecond}
	errc := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol = %s, want HTTP/2", resp.Proto)
	}
	select {
	case <-errc: //nolint:errcheck // only the write ending matters, not its error
	case <-time.After(10 * time.Second):
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
	l := &requestLimits{idle: time.Minute}
	release := make(chan struct{})
	readDone := make(chan struct{})
	reading := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &releasedBody{ReadCloser: r.Body, release: release, done: readDone}
		l.serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			go func() {
				close(reading)
				_, _ = r.Body.Read(make([]byte, 1))
			}()
			<-reading
			_, _ = io.WriteString(w, "ok")
		}))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
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
	// Give the server time to finish with the stream after the handler
	// returned, then let the read return.
	time.Sleep(200 * time.Millisecond)
	close(release)
	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the read did not return")
	}
	// A panic in the reading goroutine would kill the test binary before
	// this point; give it the time to happen.
	time.Sleep(100 * time.Millisecond)
}

// A client that stops reading the response is cut off after the idle
// timeout, and the request is cancelled.
func TestRequestLimitsIdleStalledReader(t *testing.T) {
	l := &requestLimits{idle: 200 * time.Millisecond}
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

	conn := dial(t, strings.TrimPrefix(url, "http://"), 15*time.Second)
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Fatalf("write error = %v, want a timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writing to a client that does not read was not cut off")
	}
}

func TestDeadlinesNext(t *testing.T) {
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
