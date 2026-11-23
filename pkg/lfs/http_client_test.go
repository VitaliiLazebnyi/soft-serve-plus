package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// testStallTimeout is the stall timeout of the test that waits for a
// stalled transfer to fail. The transfers that must not stall out run with
// a stall timeout of a minute instead, and check that every read and write
// pushes the deadline forward, which is what keeps a long transfer alive
// however slow it is in all, rather than timing chunks against the timeout.
const testStallTimeout = time.Second

// testPause is a pause the slow transfers take halfway, as a slow peer
// would. It is input, not a bound: it only has to be long enough for the
// clock to move, so that a deadline pushed after it is later than one set
// before it.
const testPause = 20 * time.Millisecond

// testChunks chunks of testChunk make a transfer of several reads and
// writes of the connection.
const testChunks = 30

var testChunk = strings.Repeat("c", 32<<10)

// newLoopbackAdapter returns an HTTPClient whose client is set up
// by newTransferClient like the shared secureHTTPClient, starting from the
// same whole-request timeout as ssrf.NewSecureClient, but whose transport may
// reach the loopback test server, which the SSRF-safe dialer would refuse.
// Its connections have stall as their stall timeout and record what is done
// with them in the returned log.
func newLoopbackAdapter(t *testing.T, stall time.Duration) (*HTTPClient, *connLog) {
	t.Helper()
	conns := &connLog{}
	dial := (&net.Dialer{}).DialContext
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &loggedConn{Conn: c, log: conns}, nil
	}}
	t.Cleanup(tr.CloseIdleConnections)
	client := newTransferClient(&http.Client{Timeout: ssrf.ClientTimeout, Transport: tr}, stall)
	return &HTTPClient{client: client, logger: log.New(io.Discard)}, conns
}

// connLog records, for the connections under the stall timeout, how many
// deadlines were set and the deadline each read and each write ran under.
type connLog struct {
	mu                            sync.Mutex
	deadlines                     int
	readDeadlines, writeDeadlines []time.Time
}

// check fails the test unless the connections were read and written, had a
// deadline set for each read and write, and the deadlines of the reads
// (reads true) or of the writes (reads false) moved forward over the
// transfer, which paused halfway.
func (l *connLog) check(t *testing.T, reads bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	nr, nw := len(l.readDeadlines), len(l.writeDeadlines)
	if nr == 0 || nw == 0 {
		t.Fatalf("%d reads and %d writes, want some of both", nr, nw)
	}
	if l.deadlines < nr+nw {
		t.Fatalf("%d deadlines set for %d reads and %d writes: a transfer that takes longer than the stall timeout in all would stall out", l.deadlines, nr, nw)
	}
	what, ds := "write", l.writeDeadlines
	if reads {
		what, ds = "read", l.readDeadlines
	}
	// A deadline set after the pause is later than the first one, unless
	// the deadline is not pushed forward.
	first, last := ds[0], ds[len(ds)-1]
	if !last.After(first) {
		t.Fatalf("the last %s ran under the deadline %v, not after the first one's %v: the %s does not push the deadline forward", what, last, first, what)
	}
}

// loggedConn is a connection that records in log what is done with it. A
// read or write runs under the last deadline set.
type loggedConn struct {
	net.Conn
	log      *connLog
	deadline time.Time
}

func (c *loggedConn) SetDeadline(t time.Time) error {
	c.log.mu.Lock()
	c.log.deadlines++
	c.deadline = t
	c.log.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *loggedConn) Read(b []byte) (int, error) {
	c.log.mu.Lock()
	c.log.readDeadlines = append(c.log.readDeadlines, c.deadline)
	c.log.mu.Unlock()
	return c.Conn.Read(b)
}

func (c *loggedConn) Write(b []byte) (int, error) {
	c.log.mu.Lock()
	c.log.writeDeadlines = append(c.log.writeDeadlines, c.deadline)
	c.log.mu.Unlock()
	return c.Conn.Write(b)
}

func TestHTTPClientHasNoWholeRequestTimeout(t *testing.T) {
	t.Parallel()
	c := newHTTPClient(&url.URL{}, nil)
	if c.client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want 0: it cuts off any LFS transfer that takes longer", c.client.Timeout)
	}
}

// A download is read in full, and every read of it pushes the stall
// deadline forward, so one that keeps flowing for longer than the stall
// timeout in all does not stall out.
func TestHTTPClientSlowDownloadCompletes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := range testChunks {
			if i == testChunks/2 {
				time.Sleep(testPause) // a slow server; see testPause
			}
			_, _ = io.WriteString(w, testChunk)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	a, conns := newLoopbackAdapter(t, time.Minute)
	rc, err := a.download(context.Background(), &Link{Href: srv.URL})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if want := strings.Repeat(testChunk, testChunks); string(b) != want {
		t.Fatalf("body is %d bytes, want %d", len(b), len(want))
	}
	conns.check(t, true)
}

// chunkReader yields testChunks chunks of testChunk, one a read, and
// pauses for testPause halfway.
type chunkReader struct{ left int }

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if r.left == testChunks/2 {
		time.Sleep(testPause) // a slow client; see testPause
	}
	r.left--
	return copy(p, testChunk), nil
}

// A request body is sent in full, and every write of it pushes the stall
// deadline forward although nothing is read from the server meanwhile:
// writes alone keep the connection alive.
func TestHTTPClientSlowRequestBodyCompletes(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, conns := newLoopbackAdapter(t, time.Minute)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL, &chunkReader{left: testChunks})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(testChunk) * testChunks)
	res, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	res.Body.Close() //nolint:errcheck
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if want := strings.Repeat(testChunk, testChunks); <-got != want {
		t.Fatal("server did not receive the whole request body")
	}
	conns.check(t, false)
}

// A server that stops sending must still fail the transfer, both before the
// response headers and in the middle of the body.
func TestHTTPClientStalledTransferFails(t *testing.T) {
	t.Parallel()
	for name, headers := range map[string]bool{"no headers": false, "stalled body": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if headers {
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "partial")
					w.(http.Flusher).Flush()
				}
				<-release
			}))
			defer srv.Close()
			defer close(release)

			a, _ := newLoopbackAdapter(t, testStallTimeout)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			rc, err := a.download(ctx, &Link{Href: srv.URL})
			if err == nil {
				_, err = io.ReadAll(rc)
				rc.Close() //nolint:errcheck
			}
			if err == nil {
				t.Fatal("transfer succeeded, want a stall error")
			}
			if ctx.Err() != nil {
				t.Fatal("transfer ran until the test context expired; stall timeout not applied")
			}
		})
	}
}

// closedAddr returns a loopback address nothing listens on, so a request
// to it fails with a *url.Error.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() //nolint: errcheck
	return addr
}

// debugLogger returns a logger that writes everything, down to debug, to
// the returned buffer.
func debugLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	return logger, &buf
}

// plainClient is an HTTPClient set up by newTransferClient like the shared
// secureHTTPClient, so errors pass through the stall-wrapped dialer, but
// whose transport may reach loopback test servers, which the client from
// newHTTPClient refuses.
func plainClient(t *testing.T, endpoint string) *HTTPClient {
	t.Helper()
	ep, err := NewEndpoint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{DialContext: (&net.Dialer{}).DialContext}
	t.Cleanup(tr.CloseIdleConnections)
	c := newTransferClient(&http.Client{Transport: tr}, stallTimeout)
	return &HTTPClient{client: c, endpoint: ep, logger: log.New(io.Discard)}
}

func assertNoSecrets(t *testing.T, what, s string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			t.Errorf("%s contains %q: %s", what, secret, s)
		}
	}
}

// discard is a DownloadCallback that ignores what it is given.
func discard(Pointer, io.ReadCloser, error) error { return nil }

var testPointer = Pointer{Oid: strings.Repeat("a", 64), Size: 1}

// TestBatchRedactsEndpoint checks that an endpoint's user info, which can be
// a token, is neither logged nor returned when the batch request fails.
func TestBatchRedactsEndpoint(t *testing.T) {
	t.Parallel()
	logger, logs := debugLogger()
	c := plainClient(t, "http://TOKEN:PASSWORD@"+closedAddr(t)+"/x.git")
	c.logger = logger

	err := c.Download(t.Context(), []Pointer{testPointer}, discard)
	if err == nil {
		t.Fatal("expected the batch request to fail")
	}
	var uerr *url.Error
	var operr *net.OpError
	if !errors.As(err, &uerr) || !errors.As(err, &operr) {
		t.Errorf("error lost its type: %#v", err)
	}
	assertNoSecrets(t, "error", err.Error(), "TOKEN", "PASSWORD")
	assertNoSecrets(t, "logs", logs.String(), "TOKEN", "PASSWORD")
	if !strings.Contains(logs.String(), "/x.git/info/lfs/objects/batch") {
		t.Errorf("logs no longer name the batch URL: %s", logs.String())
	}
}

// batchServer serves a batch response with the given actions for every
// object.
func batchServer(t *testing.T, actions map[string]*Link) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req BatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res := BatchResponse{Transfer: TransferBasic}
		for _, p := range req.Objects {
			res.Objects = append(res.Objects, &ObjectResponse{Pointer: p, Actions: actions})
		}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint: errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTransferRedactsHref checks that a presigned href's signature and an
// action's headers are neither logged nor returned when a transfer fails.
func TestTransferRedactsHref(t *testing.T) {
	t.Parallel()
	header := map[string]string{"Authorization": "Bearer HEADERSECRET"}
	tests := map[string]struct {
		actions  map[string]*Link
		wantURL  bool
		wantText string
		// perObject is whether the error is reported for the object rather
		// than returned.
		perObject bool
	}{
		"request fails": {
			actions: map[string]*Link{ActionDownload: {
				Href:   fmt.Sprintf("http://USER@%s/obj?X-Amz-Signature=SIGNATURE", closedAddr(t)),
				Header: header,
			}},
			wantURL:  true,
			wantText: "/obj",
		},
		"href does not parse": {
			actions: map[string]*Link{ActionDownload: {
				Href:   "http://USER@127.0.0.1/obj%zz?X-Amz-Signature=SIGNATURE",
				Header: header,
			}},
			wantURL:   true,
			perObject: true,
		},
		"missing action": {
			actions: map[string]*Link{ActionUpload: {
				Href:   "http://USER@127.0.0.1/obj?X-Amz-Signature=SIGNATURE",
				Header: header,
			}},
			wantText:  "missing action 'download'",
			perObject: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := batchServer(t, tt.actions)
			logger, logs := debugLogger()
			c := plainClient(t, srv.URL+"/x.git")
			c.logger = logger

			var objectErr error
			err := c.Download(t.Context(), []Pointer{testPointer}, func(_ Pointer, content io.ReadCloser, objectError error) error {
				if content != nil {
					content.Close() //nolint:errcheck
				}
				objectErr = objectError
				return nil
			})
			if tt.perObject {
				if err != nil {
					t.Fatalf("Download: %v", err)
				}
				err = objectErr
			} else if objectErr != nil {
				t.Fatalf("error reported for the object, want it returned: %v", objectErr)
			}
			if err == nil {
				t.Fatal("expected the download to fail")
			}
			var uerr *url.Error
			if tt.wantURL != errors.As(err, &uerr) {
				t.Errorf("error is a *url.Error = %v, want %v: %v", !tt.wantURL, tt.wantURL, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
			assertNoSecrets(t, "error", err.Error(), "USER", "SIGNATURE", "HEADERSECRET", "%zz")
			assertNoSecrets(t, "logs", logs.String(), "USER", "SIGNATURE", "HEADERSECRET", "%zz")
		})
	}
}

// TestNewHTTPClientSharesTransport checks that LFS clients share one HTTP
// client, and so one connection pool, instead of each building its own
// transport with its own idle connections.
// The clients are built from two goroutines, as concurrent mirror jobs do, so
// that -race catches newHTTPClient changing the shared client.
func TestNewHTTPClientSharesTransport(t *testing.T) {
	t.Parallel()
	var a, b *HTTPClient
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a = newHTTPClient(&url.URL{Scheme: "https", Host: "a.example.com"}, nil)
	}()
	go func() {
		defer wg.Done()
		b = newHTTPClient(&url.URL{Scheme: "https", Host: "b.example.com"}, nil)
	}()
	wg.Wait()

	if a.client != b.client {
		t.Fatal("LFS clients use separate HTTP clients; want one shared client")
	}
	if a.client.Transport != b.client.Transport {
		t.Fatal("LFS clients use separate transports; want one shared transport")
	}
}

// newHTTPClient must not set up the shared client again: wrapping the shared
// transport's dialer once more for every LFS client would stack stall
// wrappers and race with transfers already using it.
func TestNewHTTPClientDoesNotRewrapTransport(t *testing.T) {
	// Not parallel: it replaces the shared transport's dialer.
	tr, ok := secureHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("shared transport is %T, want *http.Transport", secureHTTPClient.Transport)
	}
	old := tr.DialContext
	t.Cleanup(func() { tr.DialContext = old })
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		c, s := net.Pipe()
		t.Cleanup(func() { s.Close() }) //nolint:errcheck
		return c, nil
	}

	newHTTPClient(&url.URL{Scheme: "https", Host: "a.example.com"}, nil)
	newHTTPClient(&url.URL{Scheme: "https", Host: "b.example.com"}, nil)

	conn, err := tr.DialContext(context.Background(), "tcp", "a.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	if _, ok := conn.(*stallTimeoutConn); ok {
		t.Fatal("newHTTPClient wrapped the shared transport's dialer again")
	}
}

// The shared client must be set up by newTransferClient: without it, a
// client with no whole-request timeout would have no stall limit either.
func TestSecureHTTPClientHasStallTimeout(t *testing.T) {
	t.Parallel()
	tr, ok := secureHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("shared transport is %T, want *http.Transport", secureHTTPClient.Transport)
	}
	ssrfClient := ssrf.NewSecureClient()
	plain, ok := ssrfClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("SSRF transport is %T, want *http.Transport", ssrfClient.Transport)
	}
	// A func value is not comparable, but its code pointer tells the
	// SSRF dialer apart from the stall wrapper around it.
	if reflect.ValueOf(tr.DialContext).Pointer() == reflect.ValueOf(plain.DialContext).Pointer() {
		t.Fatal("shared transport dials with the bare SSRF dialer; want it wrapped with the stall timeout")
	}
}

// Dropping the whole-request timeout for transfers keeps the transport's
// bound on the wait for response headers, so a server that takes the request
// and never answers is still given up on.
func TestSecureHTTPClientBoundsResponseHeaders(t *testing.T) {
	t.Parallel()
	tr, ok := secureHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("shared transport is %T, want *http.Transport", secureHTTPClient.Transport)
	}
	if secureHTTPClient.Timeout != 0 {
		t.Fatalf("shared client Timeout = %v, want 0", secureHTTPClient.Timeout)
	}
	if tr.ResponseHeaderTimeout != ssrf.ResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, ssrf.ResponseHeaderTimeout)
	}
}

// setTransferTimeouts must refuse a transport it cannot add the stall limit
// to, rather than leave transfers with no bound at all.
func TestSetTransferTimeoutsPanicsOnUnknownTransport(t *testing.T) {
	t.Parallel()
	for name, rt := range map[string]http.RoundTripper{
		"not an *http.Transport": http.NewFileTransport(http.Dir(".")),
		"no DialContext":         &http.Transport{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("setTransferTimeouts accepted a transport it cannot wrap")
				}
			}()
			setTransferTimeouts(&http.Client{Transport: rt}, stallTimeout)
		})
	}
}

// The shared client must still be the SSRF-protected one: it refuses to
// connect to loopback and does not follow redirects.
func TestSecureHTTPClientIsSSRFProtected(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := secureHTTPClient.Do(req)
	if err == nil {
		res.Body.Close() //nolint:errcheck
	}
	if !errors.Is(err, ssrf.ErrPrivateIP) {
		t.Fatalf("request to loopback: err = %v, want %v", err, ssrf.ErrPrivateIP)
	}

	if cr := secureHTTPClient.CheckRedirect; cr == nil || !errors.Is(cr(req, []*http.Request{req}), http.ErrUseLastResponse) {
		t.Fatal("shared client follows redirects; want it to return the redirect response")
	}
}

// objectServer serves batch responses built by respond, with base the URL
// object hrefs can point to, and objects at base+"/obj/<oid>" with serve.
// It counts the object requests per oid.
func objectServer(t *testing.T, respond func(req BatchRequest, base string) BatchResponse, serve func(w http.ResponseWriter, r *http.Request, oid string)) (*httptest.Server, func(oid string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oid, ok := strings.CutPrefix(r.URL.Path, "/obj/"); ok {
			mu.Lock()
			hits[oid]++
			mu.Unlock()
			serve(w, r, oid)
			return
		}
		var req BatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(respond(req, srv.URL)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv, func(oid string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[oid]
	}
}

// downloadLink returns the download actions for p on an objectServer.
func downloadLink(base string, p Pointer) map[string]*Link {
	return map[string]*Link{ActionDownload: {Href: base + "/obj/" + p.Oid}}
}

// downloadResult is what a DownloadCallback was given for one object.
type downloadResult struct {
	content string
	err     error
}

// collect returns a DownloadCallback that records what it is given in
// results, keyed by oid, and fails the test on an oid it gets twice.
func collect(t *testing.T, results map[string]downloadResult) DownloadCallback {
	t.Helper()
	return func(p Pointer, content io.ReadCloser, objectError error) error {
		if _, ok := results[p.Oid]; ok {
			t.Errorf("callback called twice for %s", p.Oid)
		}
		var r downloadResult
		r.err = objectError
		if content != nil {
			b, err := io.ReadAll(content)
			content.Close() //nolint:errcheck
			if err != nil {
				t.Errorf("reading %s: %v", p.Oid, err)
			}
			r.content = string(b)
		}
		results[p.Oid] = r
		return nil
	}
}

// TestDownloadTransferErrorIsPerObject checks that an object whose transfer
// fails, or that has no download action, is reported to the callback as a
// per-object error, and the objects after it are still downloaded.
func TestDownloadTransferErrorIsPerObject(t *testing.T) {
	t.Parallel()
	notFound := Pointer{Oid: strings.Repeat("1", 64), Size: 1}
	redirected := Pointer{Oid: strings.Repeat("2", 64), Size: 1}
	noAction := Pointer{Oid: strings.Repeat("3", 64), Size: 1}
	gone := Pointer{Oid: strings.Repeat("5", 64), Size: 1}
	ok := Pointer{Oid: strings.Repeat("4", 64), Size: 2}

	srv, _ := objectServer(t, func(req BatchRequest, base string) BatchResponse {
		res := BatchResponse{Transfer: TransferBasic}
		for _, p := range req.Objects {
			o := &ObjectResponse{Pointer: p, Actions: downloadLink(base, p)}
			if p == noAction {
				o.Actions = nil
			}
			res.Objects = append(res.Objects, o)
		}
		return res
	}, func(w http.ResponseWriter, r *http.Request, oid string) {
		switch oid {
		case notFound.Oid:
			http.NotFound(w, r)
		case redirected.Oid:
			http.Redirect(w, r, "/obj/"+ok.Oid, http.StatusFound)
		case gone.Oid:
			http.Error(w, "gone", http.StatusGone)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	})
	c := plainClient(t, srv.URL+"/x.git")
	// Redirects are refused, as by secureHTTPClient.
	c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	results := map[string]downloadResult{}
	if err := c.Download(t.Context(), []Pointer{notFound, redirected, noAction, gone, ok}, collect(t, results)); err != nil {
		t.Fatalf("Download: %v", err)
	}
	for _, p := range []Pointer{notFound, redirected, noAction, gone} {
		if r, found := results[p.Oid]; !found || r.err == nil {
			t.Errorf("object %s: got %+v, want a per-object error", p.Oid[:1], r)
		}
	}
	if r := results[ok.Oid]; r.err != nil || r.content != "ok" {
		t.Errorf("object after the failures: got %+v, want its content", r)
	}
}

// TestDownloadServerErrorStopsDownload checks that a download that fails
// for a reason a later try may get past, such as a 5xx, a 401 or 403 from an
// expired href, or a connection that fails, stops the download and is
// returned, instead of being reported as an error for the object, which a
// plain import would never fetch again.
func TestDownloadServerErrorStopsDownload(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusUnauthorized,
		http.StatusForbidden,
		0, // the connection fails
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			failing := Pointer{Oid: strings.Repeat("1", 64), Size: 1}
			after := Pointer{Oid: strings.Repeat("2", 64), Size: 2}
			dead := closedAddr(t)

			srv, hits := objectServer(t, func(req BatchRequest, base string) BatchResponse {
				res := BatchResponse{Transfer: TransferBasic}
				for _, p := range req.Objects {
					o := &ObjectResponse{Pointer: p, Actions: downloadLink(base, p)}
					if p == failing && status == 0 {
						o.Actions = map[string]*Link{ActionDownload: {Href: "http://" + dead + "/obj"}}
					}
					res.Objects = append(res.Objects, o)
				}
				return res
			}, func(w http.ResponseWriter, _ *http.Request, oid string) {
				if oid == failing.Oid {
					http.Error(w, "failed", status)
					return
				}
				_, _ = io.WriteString(w, "ok")
			})
			c := plainClient(t, srv.URL+"/x.git")

			results := map[string]downloadResult{}
			if err := c.Download(t.Context(), []Pointer{failing, after}, collect(t, results)); err == nil {
				t.Fatal("Download succeeded, want the transfer error")
			}
			if len(results) != 0 {
				t.Errorf("callback called with %+v, want no call", results)
			}
			if n := hits(after.Oid); n != 0 {
				t.Errorf("object after the failure downloaded %d times, want 0", n)
			}
		})
	}
}

// TestDownloadIgnoresUnrequestedObjects checks that objects in the batch
// response that were not requested, or are listed again, are neither
// downloaded nor passed to the callback. The client rejects a response with
// more objects than were requested, so this one leaves out two requested
// objects to make room for the unrequested and repeated entries.
func TestDownloadIgnoresUnrequestedObjects(t *testing.T) {
	t.Parallel()
	requested := Pointer{Oid: strings.Repeat("1", 64), Size: 2}
	unrequested := Pointer{Oid: strings.Repeat("2", 64), Size: 2}
	absent1 := Pointer{Oid: strings.Repeat("3", 64), Size: 2}
	absent2 := Pointer{Oid: strings.Repeat("4", 64), Size: 2}

	srv, hits := objectServer(t, func(_ BatchRequest, base string) BatchResponse {
		return BatchResponse{Transfer: TransferBasic, Objects: []*ObjectResponse{
			{Pointer: unrequested, Actions: downloadLink(base, unrequested)},
			{Pointer: requested, Actions: downloadLink(base, requested)},
			{Pointer: requested, Actions: downloadLink(base, requested)},
		}}
	}, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, "ok")
	})
	c := plainClient(t, srv.URL+"/x.git")

	results := map[string]downloadResult{}
	if err := c.Download(t.Context(), []Pointer{requested, absent1, absent2}, collect(t, results)); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n := hits(unrequested.Oid); n != 0 {
		t.Errorf("unrequested object downloaded %d times", n)
	}
	if n := hits(requested.Oid); n != 1 {
		t.Errorf("requested object downloaded %d times, want 1", n)
	}
	if _, found := results[unrequested.Oid]; found {
		t.Error("unrequested object passed to the callback")
	}
	if r := results[requested.Oid]; r.err != nil || r.content != "ok" {
		t.Errorf("requested object: got %+v, want its content", r)
	}
}

// TestDownloadCancelled checks that a transfer that fails because the
// context was cancelled stops the download instead of being reported as a
// per-object error.
func TestDownloadCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv, _ := objectServer(t, func(req BatchRequest, base string) BatchResponse {
		res := BatchResponse{Transfer: TransferBasic}
		for _, p := range req.Objects {
			res.Objects = append(res.Objects, &ObjectResponse{Pointer: p, Actions: downloadLink(base, p)})
		}
		return res
	}, func(_ http.ResponseWriter, r *http.Request, _ string) {
		cancel()
		<-r.Context().Done()
	})
	c := plainClient(t, srv.URL+"/x.git")

	called := false
	err := c.Download(ctx, []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Download = %v, want context.Canceled", err)
	}
	if called {
		t.Error("callback called for an object the cancellation stopped")
	}
}

// TestBatchURLKeepsQuery checks that the batch path is added to the
// endpoint's path, not appended inside its query.
func TestBatchURLKeepsQuery(t *testing.T) {
	t.Parallel()
	var got *url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(BatchResponse{Transfer: TransferBasic}) //nolint: errcheck
	}))
	t.Cleanup(srv.Close)

	c := plainClient(t, srv.URL+"/r.git/info/lfs?token=x")
	if _, err := c.batch(context.Background(), "download", []Pointer{testPointer}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("server got no request")
	}
	if got.Path != "/r.git/info/lfs/objects/batch" || got.RawQuery != "token=x" {
		t.Errorf("batch request went to %s, want /r.git/info/lfs/objects/batch?token=x", got)
	}
}

// TestBatchResponseHashAlgo checks that a batch response in a hash
// algorithm other than the sha256 the client asked for fails the transfer
// before anything is downloaded, while an empty or sha256 one is accepted.
func TestBatchResponseHashAlgo(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		algo    string
		wantErr bool
	}{
		"empty":  {algo: ""},
		"sha256": {algo: HashAlgorithmSHA256},
		"sha512": {algo: "sha512", wantErr: true},
		"SHA256": {algo: "SHA256", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				res := BatchResponse{
					Transfer: TransferBasic,
					HashAlgo: tt.algo,
					Objects: []*ObjectResponse{{
						Pointer: testPointer,
						Error:   &ObjectError{Code: http.StatusNotFound, Message: "not found"},
					}},
				}
				w.Header().Set("Content-Type", MediaType)
				json.NewEncoder(w).Encode(res) //nolint: errcheck
			}))
			t.Cleanup(srv.Close)
			c := plainClient(t, srv.URL+"/x.git")

			called := 0
			err := c.Download(context.Background(), []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error {
				called++
				return nil
			})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "hash algorithm") {
					t.Fatalf("err = %v, want a hash algorithm error", err)
				}
				if called != 0 {
					t.Fatalf("callback called %d times, want 0", called)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if called != 1 {
				t.Fatalf("callback called %d times, want 1", called)
			}
		})
	}
}

// TestNullInBatchResponse checks that a null object or link in a batch
// response fails the transfer instead of panicking.
func TestNullInBatchResponse(t *testing.T) {
	t.Parallel()
	oid := testPointer.Oid
	tests := map[string]struct {
		body     string
		wantText string
	}{
		"null object": {
			body:     `{"objects":[null]}`,
			wantText: "null object",
		},
		"null download link": {
			body:     `{"objects":[{"oid":"` + oid + `","size":1,"actions":{"download":null}}]}`,
			wantText: "invalid action 'download'",
		},
		"empty download href": {
			body:     `{"objects":[{"oid":"` + oid + `","size":1,"actions":{"download":{"href":""}}}]}`,
			wantText: "invalid action 'download'",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", MediaType)
				io.WriteString(w, tt.body) //nolint: errcheck
			}))
			t.Cleanup(srv.Close)
			c := plainClient(t, srv.URL+"/x.git")

			err := c.Download(context.Background(), []Pointer{testPointer}, discard)
			if err == nil {
				t.Fatal("expected the transfer to fail")
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
		})
	}
}
