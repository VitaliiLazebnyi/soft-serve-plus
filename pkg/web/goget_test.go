package web

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// A go get request walks up its path to the closest repository, one
// filesystem lookup per segment. An over-long path is refused before the
// walk, and a segment too long for the filesystem is not an error worth
// logging, so one anonymous request can neither burn CPU for minutes nor
// flood the error log.
//
// The backend and the server log on the test's logger, so backend lookups
// are covered too.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestGoGetLongPath(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	f := newOracleFixtureLogging(t, logger)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantCode int
	}{
		// A deep package path still resolves to its repository.
		{"deep package", "/pub/" + strings.Repeat("pkg/", 500), http.StatusOK},
		// A segment longer than any file name, under the length cap.
		{"long segment", "/pub/" + strings.Repeat("x", 300) + "/" + strings.Repeat("a/", 100), http.StatusOK},
		{"long segment missing", "/" + strings.Repeat("x", 300) + "/" + strings.Repeat("a/", 100), http.StatusNotFound},
		// Past the cap the request is refused without a single lookup.
		{"over cap", "/pub/" + strings.Repeat("a/", 10000), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			w := f.do(http.MethodGet, tc.path+"?go-get=1", "")
			if got := w.Code; got != tc.wantCode {
				t.Fatalf("w.Code = %v, want %v", got, tc.wantCode)
			}
			if tc.wantCode == http.StatusOK {
				if !strings.Contains(w.Body.String(), "/pub git ") {
					t.Fatal("not true: strings.Contains(w.Body.String(), \"/pub git \")")
				} // resolves to the repository
			}
			if strings.Contains(logs.String(), "ERRO") {
				t.Fatal("not true: !strings.Contains(logs.String(), \"ERRO\")")
			} // no error logged
			if tc.name == "over cap" {
				if strings.Contains(logs.String(), "repository name") {
					t.Fatal("not true: !strings.Contains(logs.String(), \"repository name\")")
				} // no lookup was made
			}
		})
	}
}

// allocated returns the bytes f allocates. Tests that call it must not run
// in parallel, so the count is f's own.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The go get route serves anonymous requests for any path, so the work it
// does before the length cap is checked must not grow faster than the path.
// The list of a path's parent directories, which a bearer token may name, is
// built only for a bearer token, and only for the parents nearest the root:
// one copy of the path per segment for every request would cost seconds and
// gigabytes for a 32 KB path. The go get walk tries only those parents too:
// a lookup of every parent at the length cap would cost hundreds of
// megabytes per request.
func TestGoGetPathWorkIsBounded(t *testing.T) {
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + f.mintJWT(t, f.owner, "secret")

	// Under the cap: a package path of about 2,000 segments in the
	// repository the token names.
	pkgAtCap := strings.Repeat("a/", (maxGoGetPathLen-8)/2)
	atCap := "/secret/" + pkgAtCap
	// Over the cap, as a client may send it.
	long := "/" + strings.Repeat("a/", 16<<10)

	const budget = 8 << 20
	for _, tc := range []struct {
		name, path, auth string
		wantCode         int
	}{
		{"anonymous long path", long, "", http.StatusNotFound},
		{"anonymous long go get", long + "?go-get=1", "", http.StatusNotFound},
		{"bearer long path", long, bearer, http.StatusNotFound},
		{"bearer path at cap", atCap, bearer, http.StatusNotFound},
		{"anonymous go get at cap", atCap + "?go-get=1", "", http.StatusNotFound},
		{"anonymous go get at cap found", "/pub/" + pkgAtCap + "?go-get=1", "", http.StatusOK},
		{"bearer go get at cap", atCap + "?go-get=1", bearer, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			n := allocated(func() { code = f.do(http.MethodGet, tc.path, tc.auth).Code })
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if n > budget {
				t.Errorf("allocated %d bytes, want at most %d", n, budget)
			}
		})
	}

	// A bearer token for a repository still lets its owner go get a deep
	// package path inside it.
	w := f.do(http.MethodGet, "/secret/"+strings.Repeat("pkg/", 500)+"?go-get=1", bearer)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "/secret.git") {
		t.Fatal("not true: strings.Contains(w.Body.String(), \"/secret.git\")")
	}
}

// The server reads at most maxHeaderBytes of request line and headers, not
// the net/http default of 1 MB.
func TestHTTPServerHeaderLimit(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	e := f.env
	s, err := NewHTTPServer(e.cfg, e.be, e.dbx, e.datastore, e.tokens(t), e.logger)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close() })

	// get sends the request over a raw connection and reads the status
	// line as soon as it arrives. The server closes the connection with
	// what it did not read of an over-long request still unread, which
	// some systems (Windows) answer with a reset that can discard the
	// reply; a request only a little over the limit leaves little unread.
	get := func(path string) int {
		// A loaded macOS drops a loopback connect now and then, which
		// then times out; try again, for up to closeWait, rather than fail.
		// Only a timeout is retried: any other error, such as a refused
		// connect to the test's own live listener, is a real failure and
		// fails at once, rather than being retried without a pause.
		var conn net.Conn
		var err error
		for deadline := time.Now().Add(closeWait); ; {
			conn, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", l.Addr().String()) // one attempt; the loop retries a timeout until the deadline
			var ne net.Error
			if err == nil || !errors.As(err, &ne) || !ne.Timeout() || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close() //nolint:errcheck
		if err := conn.SetDeadline(time.Now().Add(closeWait)); err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: localhost\r\n\r\n")
		}()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("/" + strings.Repeat("a", 32<<10)); got != http.StatusNotFound {
		t.Fatalf("get(\"/\"+strings.Repeat(\"a\", 32<<10)) = %v, want %v", got, http.StatusNotFound)
	}
	// net/http allows 4 KiB over MaxHeaderBytes.
	if got := get("/" + strings.Repeat("a", maxHeaderBytes+8<<10)); got != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("get(\"/\"+strings.Repeat(\"a\", maxHeaderBytes+8<<10)) = %v, want %v", got, http.StatusRequestHeaderFieldsTooLarge)
	}
}

// pathAncestors lists the parents nearest the root, and no more than
// maxPathAncestors of them.
func TestPathAncestorsBounded(t *testing.T) {
	t.Parallel()
	p := strings.TrimSuffix(strings.Repeat("a/", 1000), "/")
	got := pathAncestors(p)
	if got, want := len(got), 1+maxPathAncestors; got != want {
		t.Fatalf("len(got) = %v, want %v", got, want)
	}
	if got := got[0]; got != p {
		t.Fatalf("got[0] = %v, want %v", got, p)
	}
	if got := got[len(got)-1]; got != "a" {
		t.Fatalf("got[len(got)-1] = %v, want %v", got, "a")
	}
	if got := got[len(got)-2]; got != "a/a" {
		t.Fatalf("got[len(got)-2] = %v, want %v", got, "a/a")
	}
	if got, want := pathAncestors("a/.git/b"), ([]string{"a/.git/b", "a"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("pathAncestors(\"a/.git/b\") = %v, want %v", got, want)
	}
}

// goGet requests the go-get page for path.
func goGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path+"?go-get=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The go-get import root keeps the path of the public URL, and the page
// escapes what it interpolates.
func TestGoGetImportRoot(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.env.cfg

	cfg.HTTP.PublicURL = "https://example.com/git"
	w := goGet(t, f.handler, "/pub/sub")
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, "text/html; charset=utf-8")
	}
	body := w.Body.String()
	if !strings.Contains(body, `<meta name="go-import" content="example.com/git/pub git https://example.com/git/pub.git">`) {
		t.Fatal("not true: strings.Contains(body, `<meta name=\"go-import\" content=\"example.com/git/pub git https://example.com/git/pub.git\">`)")
	}
	if !strings.Contains(body, `href="https://pkg.go.dev/example.com/git/pub"`) {
		t.Fatal("not true: strings.Contains(body, `href=\"https://pkg.go.dev/example.com/git/pub\"`)")
	}
	if !strings.Contains(body, `content="0; url=https://pkg.go.dev/example.com/git/pub"`) {
		t.Fatal("not true: strings.Contains(body, `content=\"0; url=https://pkg.go.dev/example.com/git/pub\"`)")
	}

	cfg.HTTP.PublicURL = "https://example.com"
	body = goGet(t, f.handler, "/pub").Body.String()
	if !strings.Contains(body, `<meta name="go-import" content="example.com/pub git https://example.com/pub.git">`) {
		t.Fatal("not true: strings.Contains(body, `<meta name=\"go-import\" content=\"example.com/pub git https://example.com/pub.git\">`)")
	}

	// A public URL is configuration, but it is still not markup.
	cfg.HTTP.PublicURL = `https://example.com/a"><script>x</script>`
	w = goGet(t, f.handler, "/pub")
	body = w.Body.String()
	// The page still renders, with the value escaped rather than dropped.
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if !strings.Contains(body, `<meta name="go-import" content="example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub git https://example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub.git">`) {
		t.Fatal("not true: strings.Contains(body, `<meta name=\"go-import\" content=\"example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub git https://example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub.git\">`)")
	}
	if !strings.Contains(body, `href="https://pkg.go.dev/example.com/a%22%3E%3Cscript%3Ex%3C/script%3E/pub"`) {
		t.Fatal("not true: strings.Contains(body, `href=\"https://pkg.go.dev/example.com/a%22%3E%3Cscript%3Ex%3C/script%3E/pub\"`)")
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("not true: !strings.Contains(body, \"<script>\")")
	}
	if strings.Contains(body, `a">`) {
		t.Fatal("not true: !strings.Contains(body, `a\">`)")
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatal("not true: !strings.Contains(body, \"ZgotmplZ\")")
	}
}

func TestGoImportRoot(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://example.com":           "example.com",
		"https://example.com/":          "example.com",
		"https://example.com:8443/a/b/": "example.com:8443/a/b",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := goImportRoot(u); got != want {
			t.Fatalf("goImportRoot(u) = %v, want %v", got, want)
		}
	}
}
