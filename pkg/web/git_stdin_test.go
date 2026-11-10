package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/gorilla/mux"
)

// trackingBody records whether a read of the request body is in progress.
type trackingBody struct {
	io.ReadCloser
	reading atomic.Bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.reading.Store(true)
	defer b.reading.Store(false)
	return b.ReadCloser.Read(p)
}

// flushSignal is a response writer that reports its first Flush, which the
// service makes once git has exited, before it waits for the body.
type flushSignal struct {
	http.ResponseWriter
	once    sync.Once
	flushed chan struct{}
}

func (w *flushSignal) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *flushSignal) Flush() {
	w.once.Do(func() { close(w.flushed) })
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// handled is what the test server saw of one request.
type handled struct {
	reading  bool  // the body was still being read when serviceRpc returned
	startErr error // the request context's error when the request came in
	endErr   error // the request context's error when serviceRpc returned
}

// newServiceRpcServer serves upload-pack for a new empty repository. Each
// request's flushSignal is sent on flushes, and what the server saw of it
// on results.
func newServiceRpcServer(t *testing.T) (srv *httptest.Server, flushes <-chan *flushSignal, results <-chan handled) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()

	fc := make(chan *flushSignal, 8)
	rc := make(chan handled, 8)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := handled{startErr: r.Context().Err()}
		body := &trackingBody{ReadCloser: r.Body}
		r.Body = body
		fw := &flushSignal{ResponseWriter: w, flushed: make(chan struct{})}
		fc <- fw
		r = r.WithContext(config.WithContext(r.Context(), cfg))
		r = mux.SetURLVars(r, map[string]string{
			"service": "git-upload-pack",
			"dir":     dir,
			"repo":    "repo",
		})
		serviceRpc(fw, r)
		h.reading = body.reading.Load()
		h.endErr = r.Context().Err()
		rc <- h
	}))
	t.Cleanup(srv.Close)
	return srv, fc, rc
}

// post sends body to the server's upload-pack and reads the whole
// response. It reports whether the request went over a connection used
// before.
func post(ctx context.Context, srv *httptest.Server, body io.Reader, gzipped bool) (reused bool, err error) {
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/repo.git/git-upload-pack", body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close() //nolint: errcheck
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return false, fmt.Errorf("reading the response: %w", err)
	}
	return reused, nil
}

func waitHandled(t *testing.T, results <-chan handled) handled {
	t.Helper()
	select {
	case h := <-results:
		return h
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the handler to return")
		return handled{}
	}
}

// checkHandled fails the test if the server read the body after serviceRpc
// returned or the request context was cancelled.
func checkHandled(t *testing.T, name string, h handled) {
	t.Helper()
	if h.reading {
		t.Errorf("%s: the handler returned while the request body was still being read", name)
	}
	if h.startErr != nil {
		t.Errorf("%s: the request context was already done when it came in: %v", name, h.startErr)
	}
	if h.endErr != nil {
		t.Errorf("%s: the request context was done when the handler returned: %v", name, h.endErr)
	}
}

// A client that sends a gzip request git rejects, so git exits, and then
// keeps the request open without sending more must not leave the body being
// read after the handler returns, or the deferred close of the gzip reader
// racing that read. Waiting for it must not cancel the request context, and
// the connection must still serve the next request.
func TestServiceRpcJoinsBodyRead(t *testing.T) {
	srv, flushes, results := newServiceRpcServer(t)

	pr, pw := io.Pipe()
	defer pw.Close() //nolint: errcheck
	finish := make(chan struct{})
	go func() {
		zw := gzip.NewWriter(pw)
		// Not a pkt-line: upload-pack exits on it.
		zw.Write([]byte("zzzz")) //nolint: errcheck
		zw.Flush()               //nolint: errcheck
		// The rest of the request comes once the test lets it.
		<-finish
		zw.Close() //nolint: errcheck
		pw.Close() //nolint: errcheck
	}()

	first := make(chan error, 1)
	go func() {
		_, err := post(t.Context(), srv, pr, true)
		first <- err
	}()

	var fw *flushSignal
	select {
	case fw = <-flushes:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the request")
	}
	select {
	case <-fw.flushed:
	case h := <-results:
		t.Fatalf("the handler returned while the client was still sending: %+v", h)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for git to exit")
	}
	select {
	case h := <-results:
		t.Fatalf("the handler returned while the client was still sending: %+v", h)
	case <-time.After(100 * time.Millisecond):
	}

	// The client is done sending.
	close(finish)
	checkHandled(t, "interrupted request", waitHandled(t, results))
	if err := <-first; err != nil {
		t.Fatal(err)
	}

	// The next request reuses the connection.
	reused, err := post(t.Context(), srv, bytes.NewReader([]byte("0000")), false)
	if err != nil {
		t.Fatal(err)
	}
	<-flushes
	checkHandled(t, "next request", waitHandled(t, results))
	if !reused {
		t.Error("the connection was not reused for the next request")
	}
}

// Git can exit while the copy of a request it did not read in full is
// still running. Requests like that, sent one after another on one
// keep-alive connection as git clients do, must each start and end with a
// live context, and keep the connection.
func TestServiceRpcKeepAlive(t *testing.T) {
	srv, flushes, results := newServiceRpcServer(t)

	bodies := [][]byte{
		[]byte("0000"),
		[]byte("zzzz"),
		// More than a pipe holds, which git does not read.
		append([]byte("zzzz"), bytes.Repeat([]byte("z"), 128<<10)...),
		[]byte("0000"),
	}
	for i, b := range bodies {
		reused, err := post(t.Context(), srv, bytes.NewReader(b), false)
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		<-flushes
		checkHandled(t, fmt.Sprintf("request %d", i+1), waitHandled(t, results))
		if i > 0 && !reused {
			t.Errorf("request %d: the connection was not reused", i+1)
		}
	}
}
