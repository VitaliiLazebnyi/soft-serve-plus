package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// Error bodies are plain text that a browser must not sniff into HTML.
func TestErrorBodyIsPlainText(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	for _, path := range []string{"/nope", "/secret.git/info/refs?service=git-upload-pack"} {
		w := f.do(http.MethodGet, path, "")
		is.True(w.Code >= 400)
		is.Equal(w.Header().Get("Content-Type"), "text/plain; charset=utf-8")
		is.Equal(w.Header().Get("X-Content-Type-Options"), "nosniff")
	}
}

// Every response varies by Origin, whether or not the origin is allowed and
// however many origins are, so a shared cache does not hand one origin's
// answer to another.
func TestCORSVaryOrigin(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	cfg := config.FromContext(f.ctx)
	cfg.HTTP.CORS.AllowedOrigins = []string{"https://git.example.com"}
	h := NewRouter(f.ctx)

	for _, origin := range []string{"", "https://git.example.com", "https://evil.example.com"} {
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			req := httptest.NewRequestWithContext(context.Background(), method, "/nope", nil)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			if method == http.MethodOptions {
				req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			is.Equal(w.Header().Values("Vary"), []string{"Origin"})
		}
	}
}

// A public URL with a path still allows its origin: an Origin header never
// carries a path, so the configured URL is reduced to scheme and host.
func TestCORSPublicURLWithPath(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	cfg := config.FromContext(f.ctx)
	cfg.HTTP.PublicURL = "https://Git.Example.com:443/soft/"
	cfg.HTTP.CORS.AllowedOrigins = []string{cfg.HTTP.PublicURL}
	h := NewRouter(f.ctx)

	for origin, want := range map[string]string{
		"https://git.example.com":  "https://git.example.com",
		"https://evil.example.com": "",
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nope", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		is.Equal(w.Header().Get("Access-Control-Allow-Origin"), want)
	}
}

func TestCORSOrigins(t *testing.T) {
	is := is.New(t)
	is.Equal(corsOrigins([]string{
		"https://git.example.com/soft",
		"https://git.example.com",
		"HTTP://Example.COM:80/",
		"http://example.com:8080/x?y#z",
		"https://[::1]:443/git",
		"https://[::1]:8443",
		"*",
		"",
		"not a url",
	}), []string{
		"https://git.example.com",
		"http://example.com",
		"http://example.com:8080",
		"https://[::1]",
		"https://[::1]:8443",
		"*",
		"",
		"not a url",
	})
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
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	cfg := config.FromContext(f.ctx)

	cfg.HTTP.PublicURL = "https://example.com/git"
	w := goGet(t, f.handler, "/pub/sub")
	is.Equal(w.Code, http.StatusOK)
	is.Equal(w.Header().Get("Content-Type"), "text/html; charset=utf-8")
	body := w.Body.String()
	is.True(strings.Contains(body, `<meta name="go-import" content="example.com/git/pub git https://example.com/git/pub.git">`))
	is.True(strings.Contains(body, `href="https://pkg.go.dev/example.com/git/pub"`))
	is.True(strings.Contains(body, `content="0; url=https://pkg.go.dev/example.com/git/pub"`))

	cfg.HTTP.PublicURL = "https://example.com"
	body = goGet(t, f.handler, "/pub").Body.String()
	is.True(strings.Contains(body, `<meta name="go-import" content="example.com/pub git https://example.com/pub.git">`))

	// A public URL is configuration, but it is still not markup.
	cfg.HTTP.PublicURL = `https://example.com/a"><script>x</script>`
	w = goGet(t, f.handler, "/pub")
	body = w.Body.String()
	// The page still renders, with the value escaped rather than dropped.
	is.Equal(w.Code, http.StatusOK)
	is.True(strings.Contains(body, `<meta name="go-import" content="example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub git https://example.com/a&#34;&gt;&lt;script&gt;x&lt;/script&gt;/pub.git">`))
	is.True(strings.Contains(body, `href="https://pkg.go.dev/example.com/a%22%3E%3Cscript%3Ex%3C/script%3E/pub"`))
	is.True(!strings.Contains(body, "<script>"))
	is.True(!strings.Contains(body, `a">`))
	is.True(!strings.Contains(body, "ZgotmplZ"))
}

func TestGoImportRoot(t *testing.T) {
	is := is.New(t)
	for raw, want := range map[string]string{
		"https://example.com":           "example.com",
		"https://example.com/":          "example.com",
		"https://example.com:8443/a/b/": "example.com:8443/a/b",
	} {
		u, err := url.Parse(raw)
		is.NoErr(err)
		is.Equal(goImportRoot(u), want)
	}
}

// A lock is refused for a path that does not name one file in the working
// tree, and nothing is stored for it.
func TestLFSLockCreateRejectsInvalidPath(t *testing.T) {
	ctx := access.WithContext(newLocksParamsContext(t), access.ReadWriteAccess)
	dbx := db.FromContext(ctx)
	countLocks := func() int {
		t.Helper()
		var n int
		if err := dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM lfs_locks"); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countLocks()

	for _, p := range []string{"", "/a.bin", "../a.bin", "./a.bin", "dir//a.bin", "dir/../a.bin", "a\x00b"} {
		t.Run(p, func(t *testing.T) {
			b, _ := json.Marshal(lfs.LockCreateRequest{Path: p, Ref: lfs.Reference{Name: "refs/heads/main"}})
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks", strings.NewReader(string(b)))
			req.Header.Set("Accept", lfs.MediaType)
			req.Header.Set("Content-Type", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnprocessableEntity, w.Body)
			}
		})
	}

	if after := countLocks(); after != before {
		t.Fatalf("locks = %d, want %d", after, before)
	}

	// A valid path still locks.
	b, _ := json.Marshal(lfs.LockCreateRequest{Path: "dir/c.bin", Ref: lfs.Reference{Name: "refs/heads/main"}})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks", strings.NewReader(string(b)))
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Content-Type", lfs.MediaType)
	w := httptest.NewRecorder()
	serviceLfsLocks(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body)
	}
}
