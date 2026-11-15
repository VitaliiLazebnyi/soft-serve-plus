package web

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A go get request walks up its path to the closest repository, one
// filesystem lookup per segment. An over-long path is refused before the
// walk, and a segment too long for the filesystem is not an error worth
// logging, so one anonymous request can neither burn CPU for minutes nor
// flood the error log.
//
// Backend lookups log through the logger captured when the backend is
// created, which falls back to the global default, so the test swaps the
// global default logger and must not run in parallel.
func TestGoGetLongPath(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	prev := log.Default()
	log.SetDefault(logger)
	t.Cleanup(func() { log.SetDefault(prev) })

	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

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
			is := is.New(t)
			logs.Reset()
			w := f.do(http.MethodGet, tc.path+"?go-get=1", "")
			is.Equal(w.Code, tc.wantCode)
			if tc.wantCode == http.StatusOK {
				is.True(strings.Contains(w.Body.String(), "/pub git ")) // resolves to the repository
			}
			is.True(!strings.Contains(logs.String(), "ERRO")) // no error logged
			if tc.name == "over cap" {
				is.True(!strings.Contains(logs.String(), "repository name")) // no lookup was made
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
// The list of a path's parent directories, which a bearer token may name,
// used to be built for every request, one copy of the path per segment: a
// 32 KB path cost seconds and gigabytes. It is now built only for a bearer
// token, and only for the parents nearest the root.
func TestGoGetPathWorkIsBounded(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	bearer := "Bearer " + mintJWT(f.ctx, t, f.owner, "secret")

	// Under the cap: a package path of about 2,000 segments in the
	// repository the token names.
	atCap := "/secret/" + strings.Repeat("a/", (maxGoGetPathLen-8)/2)
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
	is.Equal(w.Code, http.StatusOK)
	is.True(strings.Contains(w.Body.String(), "/secret.git"))
}

// The server reads at most maxHeaderBytes of request line and headers, not
// the net/http default of 1 MB.
func TestHTTPServerHeaderLimit(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	s, err := NewHTTPServer(f.ctx)
	is.NoErr(err)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	is.NoErr(err)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close() })

	get := func(path string) int {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+l.Addr().String()+path, nil)
		is.NoErr(err)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		is.NoErr(err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	is.Equal(get("/"+strings.Repeat("a", 32<<10)), http.StatusNotFound)
	is.Equal(get("/"+strings.Repeat("a", 128<<10)), http.StatusRequestHeaderFieldsTooLarge)
}

// pathAncestors lists the parents nearest the root, and no more than
// maxPathAncestors of them.
func TestPathAncestorsBounded(t *testing.T) {
	is := is.New(t)
	p := strings.TrimSuffix(strings.Repeat("a/", 1000), "/")
	got := pathAncestors(p)
	is.Equal(len(got), 1+maxPathAncestors)
	is.Equal(got[0], p)
	is.Equal(got[len(got)-1], "a")
	is.Equal(got[len(got)-2], "a/a")
	is.Equal(pathAncestors("a/.git/b"), []string{"a/.git/b", "a"})
}
