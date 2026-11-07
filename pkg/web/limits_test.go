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

// newLimitsServer serves the real git router, built with the HTTP limits
// configure sets, over a real connection, so deadlines apply.
func newLimitsServer(t *testing.T, configure func(*config.HTTPConfig)) string {
	t.Helper()
	ctx, be, _ := newLFSTestContext(t)
	if _, err := be.CreateRepository(ctx, "repo", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	configure(&config.FromContext(ctx).HTTP)
	srv := httptest.NewUnstartedServer(lfsTestRouter(ctx))
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

	status := func() int {
		t.Helper()
		conn := dial(t, addr, 10*time.Second)
		defer conn.Close() //nolint:errcheck
		if _, err := io.WriteString(conn, "GET /repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\n"+
			"Host: localhost\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := status(); got != http.StatusOK {
		t.Fatalf("status before the slot is taken = %d, want %d", got, http.StatusOK)
	}

	stalled := dial(t, addr, 30*time.Second)
	sendStalled(t, stalled)
	waitFor(t, func() bool { return status() == http.StatusServiceUnavailable },
		"a request over max_connections was not turned away")

	_ = stalled.Close()
	waitFor(t, func() bool { return status() == http.StatusOK },
		"the slot was not freed once the stalled request ended")
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
