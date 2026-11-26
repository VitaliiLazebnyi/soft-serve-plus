package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ownerAuth returns an Authorization header for the fixture's owner.
func (f *oracleFixture) ownerAuth(t *testing.T) string {
	t.Helper()
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "owner", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return "Token " + token
}

// send performs a request with the given headers and body.
func (f *oracleFixture) send(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// Auth-params of a challenge are separated by commas (RFC 7235).
func TestCredentialsChallengeSyntax(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	w := f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", "")
	if got := w.Code; got != http.StatusUnauthorized {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusUnauthorized)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="Git", charset="UTF-8", Token, Bearer` {
		t.Fatalf("w.Header().Get(\"WWW-Authenticate\") = %v, want %v", got, `Basic realm="Git", charset="UTF-8", Token, Bearer`)
	}
	if got := w.Header().Get("LFS-Authenticate"); got != `Basic realm="Git LFS", charset="UTF-8", Token, Bearer` {
		t.Fatalf("w.Header().Get(\"LFS-Authenticate\") = %v, want %v", got, `Basic realm="Git LFS", charset="UTF-8", Token, Bearer`)
	}
}

// A wrong method gets 405 with an Allow header whatever the protocol
// version, and a route that answers GET answers HEAD too.
func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)

	for _, tc := range []struct {
		name         string
		major, minor int
	}{{"HTTP/1.0", 1, 0}, {"HTTP/1.1", 1, 1}, {"HTTP/2.0", 2, 0}} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/secret.git/info/lfs/objects/batch", nil)
		req.Proto, req.ProtoMajor, req.ProtoMinor = tc.name, tc.major, tc.minor
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		if got := w.Code; got != http.StatusMethodNotAllowed {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusMethodNotAllowed)
		}
		if got := w.Header().Get("Allow"); got != http.MethodPost {
			t.Fatalf("w.Header().Get(\"Allow\") = %v, want %v", got, http.MethodPost)
		}
	}

	w := f.do(http.MethodPost, "/secret.git/info/refs", auth)
	if got := w.Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("w.Header().Get(\"Allow\") = %v, want %v", got, "GET, HEAD")
	}

	if got := f.do(http.MethodHead, "/secret.git/info/refs?service=git-upload-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodHead, \"/secret.git/info/refs?service=git-upload-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}

	// HEAD on the push advertisement of a missing repository does not
	// create it, though GET would for a caller who may push.
	if got := f.do(http.MethodHead, "/fresh.git/info/refs?service=git-receive-pack", "Token "+f.token).Code; got != http.StatusNotFound {
		t.Fatalf("f.do(http.MethodHead, \"/fresh.git/info/refs?service=git-receive-pack\", \"Token \"+f.token).Code = %v, want %v", got, http.StatusNotFound)
	}
	if f.exists("fresh") {
		t.Fatal("not true: !f.exists(\"fresh\")")
	}
}

// The RPC endpoints answer 415 to any body but the service's request type,
// as git http-backend does, and their responses are not cached.
func TestServiceRPCContentType(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)

	for _, ct := range []string{"", "text/plain", "application/x-git-upload-pack-requestx", "application/x-git-receive-pack-request"} {
		w := f.send(http.MethodPost, "/secret.git/git-upload-pack", map[string]string{"Authorization": auth, "Content-Type": ct}, "")
		if got := w.Code; got != http.StatusUnsupportedMediaType {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusUnsupportedMediaType)
		}
	}

	for _, ct := range []string{"application/x-git-upload-pack-request", "application/x-git-upload-pack-request; charset=utf-8"} {
		w := f.send(http.MethodPost, "/secret.git/git-upload-pack", map[string]string{"Authorization": auth, "Content-Type": ct}, "0000")
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		if got := w.Header().Get("Content-Type"); got != "application/x-git-upload-pack-result" {
			t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, "application/x-git-upload-pack-result")
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache, max-age=0, must-revalidate" {
			t.Fatalf("w.Header().Get(\"Cache-Control\") = %v, want %v", got, "no-cache, max-age=0, must-revalidate")
		}
	}
}

// signJWT signs claims with e's host key, using the algorithm of its key
// type the way `soft jwt` does.
func signJWT(t *testing.T, e *testEnv, claims jwt.RegisteredClaims) string {
	t.Helper()
	signed, err := e.tokens(t).SignClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// A bearer token must expire.
func TestBearerTokenRequiresExpiry(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:  fmt.Sprintf("%s#%d", f.owner.Username(), f.owner.ID()),
		IssuedAt: jwt.NewNumericDate(now),
		Issuer:   f.env.cfg.HTTP.PublicURL,
		Audience: []string{"secret"},
	}
	const path = "/secret.git/info/refs?service=git-upload-pack"
	if got := f.do(http.MethodGet, path, "Bearer "+signJWT(t, f.env, claims)).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, path, \"Bearer \"+signJWT(t, f.env, claims)).Code = %v, want %v", got, http.StatusForbidden)
	}

	claims.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour))
	if got := f.do(http.MethodGet, path, "Bearer "+signJWT(t, f.env, claims)).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, path, \"Bearer \"+signJWT(t, f.env, claims)).Code = %v, want %v", got, http.StatusOK)
	}
}

// go get of a package inside a repository accepts a token minted for the
// repository, but not one minted for another repository.
func TestGoGetBearerTokenForParentRepository(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)

	for _, path := range []string{"/secret?go-get=1", "/secret/sub/pkg?go-get=1", "/secret.git/sub?go-get=1"} {
		w := f.do(http.MethodGet, path, "Bearer "+f.mintJWT(t, f.owner, "secret"))
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), `name="go-import"`) {
			t.Fatal("not true: strings.Contains(w.Body.String(), `name=\"go-import\"`)")
		}
		if !strings.Contains(w.Body.String(), "https://pkg.go.dev/") {
			t.Fatal("not true: strings.Contains(w.Body.String(), \"https://pkg.go.dev/\")")
		}
	}
	if got := f.do(http.MethodGet, "/secret/sub?go-get=1", "Bearer "+f.mintJWT(t, f.owner, "other")).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/secret/sub?go-get=1\", \"Bearer \"+f.mintJWT(t, f.owner, \"other\")).Code = %v, want %v", got, http.StatusForbidden)
	}
	if got := f.do(http.MethodGet, "/secret/sub?go-get=1", "Bearer "+f.mintJWT(t, f.owner, "secret/sub/pkg")).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/secret/sub?go-get=1\", \"Bearer \"+f.mintJWT(t, f.owner, \"secret/sub/pkg\")).Code = %v, want %v", got, http.StatusForbidden)
	}

	// A token for "secret" does not reveal the nested repository
	// "secret/sub", even to a caller who may read it: the walk skips it
	// and answers for "secret".
	_, err := f.be.CreateRepository(f.ctx, "secret/sub", f.owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/secret/sub?go-get=1", "/secret/sub/pkg?go-get=1"} {
		w := f.do(http.MethodGet, path, "Bearer "+f.mintJWT(t, f.owner, "secret"))
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "/secret.git") {
			t.Fatal("not true: strings.Contains(w.Body.String(), \"/secret.git\")")
		}
		if strings.Contains(w.Body.String(), "secret/sub") {
			t.Fatal("not true: !strings.Contains(w.Body.String(), \"secret/sub\")")
		}

		w = f.do(http.MethodGet, path, "Bearer "+f.mintJWT(t, f.owner, "secret/sub"))
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "/secret/sub.git") {
			t.Fatal("not true: strings.Contains(w.Body.String(), \"/secret/sub.git\")")
		}
	}
}

func TestPathAncestors(t *testing.T) {
	t.Parallel()
	if got, want := pathAncestors("a"), ([]string{"a"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("pathAncestors(\"a\") = %v, want %v", got, want)
	}
	if got, want := pathAncestors("a.git/b/c"), ([]string{"a.git/b/c", "a.git/b", "a"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("pathAncestors(\"a.git/b/c\") = %v, want %v", got, want)
	}
}

// LFS JSON endpoints answer media type errors in JSON: 406 for the Accept
// header and 415 for the Content-Type header. Responses carry no Accept
// header.
func TestLFSMediaTypes(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)
	const batch = "/secret.git/info/lfs/objects/batch"
	body := `{"operation":"download","objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]}`

	for _, tc := range []struct {
		accept, contentType string
		code                int
	}{
		{"", lfs.MediaType, http.StatusNotAcceptable},
		{"text/plain", lfs.MediaType, http.StatusNotAcceptable},
		{lfs.MediaType, "", http.StatusUnsupportedMediaType},
		{lfs.MediaType, "application/json", http.StatusUnsupportedMediaType},
		{lfs.MediaType, lfs.MediaType + "x", http.StatusUnsupportedMediaType},
		{lfs.MediaType, lfs.MediaType + "; charset=utf-8", http.StatusOK},
	} {
		w := f.send(http.MethodPost, batch, map[string]string{"Authorization": auth, "Accept": tc.accept, "Content-Type": tc.contentType}, body)
		if got := w.Code; got != tc.code {
			t.Fatalf("w.Code = %v, want %v", got, tc.code)
		}
		if got := w.Header().Get("Content-Type"); got != lfs.MediaType {
			t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, lfs.MediaType)
		}
		if got := w.Header().Get("Accept"); got != "" {
			t.Fatalf("w.Header().Get(\"Accept\") = %v, want %v", got, "")
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
	}

	w := f.send(http.MethodGet, "/secret.git/info/lfs/locks", map[string]string{"Authorization": auth}, "")
	if got := w.Code; got != http.StatusNotAcceptable {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusNotAcceptable)
	}
	if got := w.Header().Get("Content-Type"); got != lfs.MediaType {
		t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, lfs.MediaType)
	}
}

// The batch API answers 409 to a hash algorithm other than SHA-256.
func TestLFSBatchHashAlgo(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)
	hdr := map[string]string{"Authorization": auth, "Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	objects := `"objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]`

	w := f.send(http.MethodPost, "/secret.git/info/lfs/objects/batch", hdr, `{"operation":"download","hash_algo":"sha512",`+objects+`}`)
	if got := w.Code; got != http.StatusConflict {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusConflict)
	}
	if got := w.Header().Get("Content-Type"); got != lfs.MediaType {
		t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, lfs.MediaType)
	}

	w = f.send(http.MethodPost, "/secret.git/info/lfs/objects/batch", hdr, `{"operation":"download","hash_algo":"sha256",`+objects+`}`)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
}

// Bad credentials get the same JSON 403 from LFS whether or not the caller
// could read the repository anonymously.
func TestLFSBadCredentialsAreJSON(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "public", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	badBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("owner:wrong-password"))
	hdr := map[string]string{"Authorization": badBasic, "Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	var want string
	for _, repo := range []string{"secret", "public"} {
		w := f.send(http.MethodPost, "/"+repo+".git/info/lfs/objects/batch", hdr, `{}`)
		if got := w.Code; got != http.StatusForbidden {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusForbidden)
		}
		if got := w.Header().Get("Content-Type"); got != lfs.MediaType {
			t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, lfs.MediaType)
		}
		if want == "" {
			want = w.Body.String()
		}
		if got := w.Body.String(); got != want {
			t.Fatalf("w.Body.String() = %v, want %v", got, want)
		}
	}
}

// withParams leaves the request URL alone, so request logs show the path
// the client asked for.
func TestWithParamsKeepsURL(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	var inner string
	router := mux.NewRouter()
	router.Handle("/{repo:.*}/info/refs", withParams(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inner = r.URL.Path
		if got := mux.Vars(r)["repo"]; got != "a/b" {
			t.Fatalf("mux.Vars(r)[\"repo\"] = %v, want %v", got, "a/b")
		}
	})))
	req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/a/b.git/info/refs", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	if got := inner; got != "/a/b.git/info/refs" {
		t.Fatalf("inner = %v, want %v", got, "/a/b.git/info/refs")
	}
	if got := req.URL.Path; got != "/a/b.git/info/refs" {
		t.Fatalf("req.URL.Path = %v, want %v", got, "/a/b.git/info/refs")
	}
}

// A panic is answered with 500 and logged with its stack on the request
// logger.
func TestRecoverPanics(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)
	s := newServer(config.DefaultConfig(), nil, nil, nil, nil, logger)
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Code; got != http.StatusInternalServerError {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusInternalServerError)
	}
	out := logs.String()
	if !strings.Contains(out, "boom") {
		t.Fatal("not true: strings.Contains(out, \"boom\")")
	}
	if !strings.Contains(out, "path=/x") {
		t.Fatal("not true: strings.Contains(out, \"path=/x\")")
	}
	if !strings.Contains(out, "TestRecoverPanics") {
		t.Fatal("not true: strings.Contains(out, \"TestRecoverPanics\")")
	}

	// http.ErrAbortHandler still aborts the response.
	abort := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if got, want := recover(), http.ErrAbortHandler; !reflect.DeepEqual(got, want) {
			t.Fatalf("recover() = %v, want %v", got, want)
		}
	}()
	abort.ServeHTTP(httptest.NewRecorder(), req)
}

// Failed TLS handshakes are logged at debug level; other server errors
// stay errors.
func TestServerErrorLog(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.InfoLevel)
	l := stdlog.New(serverErrorLog{logger}, "", 0)

	l.Printf("http: TLS handshake error from 127.0.0.1:1234: EOF")
	if got := logs.String(); got != "" {
		t.Fatalf("logs.String() = %v, want %v", got, "")
	}

	l.Printf("http: Accept error: boom")
	if !strings.Contains(logs.String(), "ERRO") {
		t.Fatal("not true: strings.Contains(logs.String(), \"ERRO\")")
	}
	if !strings.Contains(logs.String(), "http: Accept error: boom") {
		t.Fatal("not true: strings.Contains(logs.String(), \"http: Accept error: boom\")")
	}
}

// TestAuthorize pins the decision order for every kind of caller against
// every kind of route and repository state. The property that matters for
// the existence oracle is that, for a caller who cannot read, the verdict
// for a private repository (!readable, NoAccess) equals the verdict for a
// missing one (!readable, whatever level the caller gets for a missing
// name), so the level alone must not change the answer.
func TestAuthorize(t *testing.T) {
	t.Parallel()
	type route struct {
		name string
		need routeAccess
	}
	isLFS := func(rt route) bool { return rt.need.lfs }
	infoRefs := gitRouteFor(t, "/info/refs")
	rpc := gitRouteFor(t, "/{service:(?:git-upload-archive|git-upload-pack|git-receive-pack)$}")
	basic := gitRouteFor(t, "/info/lfs/objects/basic/{oid:[0-9a-f]{64}$}")
	routes := []route{
		{"fetch advert", infoRefs.access(http.MethodGet, gitserve.UploadPackService)},
		{"fetch rpc", rpc.access(http.MethodPost, gitserve.UploadPackService)},
		{"push advert", infoRefs.access(http.MethodGet, gitserve.ReceivePackService)},
		{"push rpc", rpc.access(http.MethodPost, gitserve.ReceivePackService)},
		{"lfs batch", gitRouteFor(t, "/info/lfs/objects/batch").access(http.MethodPost, "")},
		{"lfs upload", basic.access(http.MethodPut, "")},
		{"lfs lock", gitRouteFor(t, "/info/lfs/locks").access(http.MethodPost, "")},
		{"push advert head", infoRefs.access(http.MethodHead, gitserve.ReceivePackService)},
	}

	type caller struct {
		name      string
		anonymous bool
		badCreds  bool
		// level the caller gets for a missing repository
		missing access.Level
	}
	callers := []caller{
		{"anonymous", true, false, access.ReadOnlyAccess},
		{"anonymous no-access", true, false, access.NoAccess},
		{"bad credentials", true, true, access.ReadOnlyAccess},
		{"signed-in", false, false, access.ReadWriteAccess},
	}

	// Missing and private must be indistinguishable for every caller who
	// cannot read the private repository.
	for _, c := range callers {
		for _, rt := range routes {
			private := authorize(rt.need, false, access.NoAccess, c.anonymous, c.badCreds)
			missing := authorize(rt.need, false, c.missing, c.anonymous, c.badCreds)
			want := denyUnreadable
			switch {
			case c.badCreds && !isLFS(rt):
				// LFS renders bad credentials from denyRead so the body
				// is JSON; the verdict stays denyUnreadable.
				want = forbid
			case rt.need.create && !c.anonymous:
				// The one inherent difference: a push to a new name by a
				// user who may create repositories creates it.
				if missing != createThenServe {
					t.Errorf("%s/%s: missing: got %v, want createThenServe", c.name, rt.name, missing)
				}
				if private != denyUnreadable {
					t.Errorf("%s/%s: private: got %v, want denyUnreadable", c.name, rt.name, private)
				}
				continue
			}
			if private != want || missing != want {
				t.Errorf("%s/%s: private=%v missing=%v, want both %v", c.name, rt.name, private, missing, want)
			}
		}
	}

	// Readers and writers of an existing repository.
	tests := []struct {
		name      string
		route     route
		level     access.Level
		anonymous bool
		want      verdict
	}{
		{"anon reader fetch", routes[0], access.ReadOnlyAccess, true, serve},
		{"anon reader push", routes[2], access.ReadOnlyAccess, true, unauthorized},
		{"anon reader push rpc", routes[3], access.ReadOnlyAccess, true, unauthorized},
		{"anon reader lfs batch", routes[4], access.ReadOnlyAccess, true, serve},
		{"anon reader lfs upload", routes[5], access.ReadOnlyAccess, true, unauthorizedLFS},
		{"anon reader lfs lock", routes[6], access.ReadOnlyAccess, true, unauthorizedLFS},
		{"reader fetch", routes[0], access.ReadOnlyAccess, false, serve},
		{"reader push", routes[2], access.ReadOnlyAccess, false, forbid},
		{"reader push rpc", routes[3], access.ReadOnlyAccess, false, forbid},
		{"reader lfs upload", routes[5], access.ReadOnlyAccess, false, forbidLFSWrite},
		{"writer push", routes[2], access.ReadWriteAccess, false, serve},
		{"writer push rpc", routes[3], access.ReadWriteAccess, false, serve},
		{"writer lfs upload", routes[5], access.ReadWriteAccess, false, serve},
		{"writer lfs lock", routes[6], access.ReadWriteAccess, false, serve},
		{"anon writer push", routes[2], access.ReadWriteAccess, true, serve},
		{"writer push head", routes[7], access.ReadWriteAccess, false, serve},
	}
	for _, tt := range tests {
		got := authorize(tt.route.need, true, tt.level, tt.anonymous, false)
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}

	// Bad credentials on a readable repository are refused, and never
	// create anything even when the anonymous level would allow a push.
	for _, rt := range routes {
		if got := authorize(rt.need, true, access.ReadOnlyAccess, true, true); got != forbid {
			t.Errorf("bad creds on readable %s: got %v, want forbid", rt.name, got)
		}
		want := forbid
		if isLFS(rt) {
			want = denyUnreadable
		}
		if got := authorize(rt.need, false, access.ReadWriteAccess, true, true); got != want {
			t.Errorf("bad creds on missing %s: got %v, want %v", rt.name, got, want)
		}
	}

	// A push may only create from the push endpoints, and not on HEAD.
	if got := authorize(routeAccess{write: true}, false, access.ReadWriteAccess, false, false); got != denyUnreadable {
		t.Errorf("write without create: got %v, want denyUnreadable", got)
	}
	for _, rt := range gitRoutes {
		for _, method := range rt.allowed() {
			for _, service := range []gitserve.Service{"", gitserve.UploadPackService, gitserve.UploadArchiveService, gitserve.ReceivePackService} {
				need := rt.access(method, service)
				wantCreate := rt.service && service == gitserve.ReceivePackService && method != http.MethodHead
				if need.create != wantCreate {
					t.Errorf("%s %s %q: create = %v, want %v", method, rt.path, service, need.create, wantCreate)
				}
			}
		}
	}
}

// anonFailsAfterStore answers the first ok reads of the anon-access setting
// since reads was last reset, and fails every one after, as a database that
// goes down between the request's access check and the create would.
type anonFailsAfterStore struct {
	store.Store
	ok, reads *atomic.Int32
}

func (s anonFailsAfterStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	if s.reads.Add(1) > s.ok.Load() {
		return access.NoAccess, errors.New("SQL logic error: no such table: settings")
	}
	return s.Store.GetAnonAccess(ctx, h)
}

// A push-to-create whose creation check cannot read the settings is
// answered 503 with Retry-After and no credentials challenge, as for any
// failed lookup, not 500, and nothing is created.
func TestPushCreateLookupFailureUnavailable(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx := env.ctx
	ok, reads := &atomic.Int32{}, &atomic.Int32{}
	ok.Store(math.MaxInt32)
	env.useStore(anonFailsAfterStore{Store: env.datastore, ok: ok, reads: reads})
	be := env.be

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, alice, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	router := env.router(t)

	// The request's access check reads the setting once; the create's
	// check is the read that fails.
	reads.Store(0)
	ok.Store(1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/new.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Authorization", "Token "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get("Retry-After"); got != authRetryAfter {
		t.Fatalf("w.Header().Get(\"Retry-After\") = %v, want %v", got, authRetryAfter)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("w.Header().Get(\"WWW-Authenticate\") = %v, want %v", got, "")
	}

	ok.Store(math.MaxInt32)
	_, err = be.Repository(ctx, "new")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
}

// TestDumbHTTPIsRefused checks every request of the dumb HTTP protocol gets
// one uncacheable 403, whatever the repository and the caller, even for a
// missing loose object.
func TestDumbHTTPIsRefused(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pubDir := filepath.Join(f.env.cfg.DataPath, "repos", "pub.git")
	looseObj := "objects/12/" + strings.Repeat("3", 38)
	if err := os.MkdirAll(filepath.Join(pubDir, "objects", "12"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pubDir, filepath.FromSlash(looseObj)), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := []string{
		"info/refs",
		"info/refs?service=git-upload-archive",
		"info/refs?service=bogus",
		"HEAD",
		"HEAD?service=git-receive-pack",
		"objects/info/packs",
		"objects/info/alternates",
		"objects/info/http-alternates",
		looseObj,
		"objects/ab/" + strings.Repeat("0", 38),
		"objects/ab/" + strings.Repeat("0", 62),
		"objects/pack/pack-" + strings.Repeat("5", 40) + ".pack",
		"objects/pack/pack-" + strings.Repeat("5", 64) + ".idx",
	}
	auths := []string{"", "Token " + f.token, "Token garbage", "Bearer garbage"}
	for _, repo := range []string{"pub", "secret", "ghost"} {
		for _, file := range files {
			for _, auth := range auths {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					w := f.do(method, "/"+repo+".git/"+file, auth)
					if got := w.Code; got != http.StatusForbidden {
						t.Fatalf("w.Code = %v, want %v", got, http.StatusForbidden)
					} // dumb request
					if got := w.Header().Get("Cache-Control"); got != "no-cache, max-age=0, must-revalidate" {
						t.Fatalf("w.Header().Get(\"Cache-Control\") = %v, want %v", got, "no-cache, max-age=0, must-revalidate")
					}
					if got := w.Header().Get("Expires"); got != "Fri, 01 Jan 1980 00:00:00 GMT" {
						t.Fatalf("w.Header().Get(\"Expires\") = %v, want %v", got, "Fri, 01 Jan 1980 00:00:00 GMT")
					}
					if got := w.Header().Get("WWW-Authenticate"); got != "" {
						t.Fatalf("w.Header().Get(\"WWW-Authenticate\") = %v, want %v", got, "")
					} // no credentials help
					if method == http.MethodGet {
						if got, want := w.Body.String(), errDumbHTTP.Error()+"\n"; got != want {
							t.Fatalf("w.Body.String() = %v, want %v", got, want)
						}
					}
				}
			}
		}
	}

	// Other methods are refused too, and nothing was created.
	w := f.do(http.MethodPost, "/pub.git/HEAD", "")
	if got := w.Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("w.Header().Get(\"Allow\") = %v, want %v", got, "GET, HEAD")
	}
	_, err = f.be.Repository(f.ctx, "ghost")
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	// A smart client still gets the advertisement.
	if got := f.do(http.MethodGet, "/pub.git/info/refs?service=git-upload-pack", "").Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/pub.git/info/refs?service=git-upload-pack\", \"\").Code = %v, want %v", got, http.StatusOK)
	}
}

// TestDumbCloneFails checks a client forced to the dumb protocol is told why
// it cannot clone.
func TestDumbCloneFails(t *testing.T) {
	t.Parallel()
	f := newInfoFixture(t)
	f.populate(t)

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "clone.git")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "--bare", srv.URL+"/pub", dst)
	cmd.Env = append(os.Environ(), "GIT_SMART_HTTP=0", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("dumb clone succeeded: %s", out)
	}
	if !strings.Contains(string(out), "403") {
		t.Errorf("dumb clone output does not show the 403:\n%s", out)
	}
}

// TestGoGetPathEndingInDumbFile checks a go get request for a repository or
// package whose path ends in a dumb HTTP file name, such as "group/HEAD" or
// "pub/info/refs", gets its go-import page rather than the dumb HTTP
// refusal.
func TestGoGetPathEndingInDumbFile(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "group/HEAD", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.be.CreateRepository(f.ctx, "deep/info/refs", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ path, repo string }{
		{"/group/HEAD?go-get=1", "group/HEAD"},
		{"/group/HEAD/objects/info/packs?go-get=1", "group/HEAD"}, // a package inside it
		{"/pub/info/refs?go-get=1", "pub"},                        // a package "info/refs"
		{"/pub.git/info/refs?go-get=1", "pub"},
		{"/deep/info/refs?go-get=1", "deep/info/refs"}, // a repository "info/refs"
	} {
		w := f.do(http.MethodGet, tc.path, "")
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		} // go get page
		if !strings.Contains(w.Body.String(), `name="go-import"`) {
			t.Fatal("not true: strings.Contains(w.Body.String(), `name=\"go-import\"`)")
		}
		if !strings.Contains(w.Body.String(), "/"+tc.repo+" git ") {
			t.Fatal("not true: strings.Contains(w.Body.String(), \"/\"+tc.repo+\" git \")")
		}
	}

	// Without go-get=1, and with another method, it is still refused.
	if got := f.do(http.MethodGet, "/group/HEAD", "").Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/group/HEAD\", \"\").Code = %v, want %v", got, http.StatusForbidden)
	}
	if got := f.do(http.MethodGet, "/pub/info/refs", "").Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/pub/info/refs\", \"\").Code = %v, want %v", got, http.StatusForbidden)
	}
	// A smart ref advertisement is still one, go-get=1 or not.
	if got := f.do(http.MethodGet, "/pub/info/refs?service=git-upload-pack&go-get=1", "").Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/pub/info/refs?service=git-upload-pack&go-get=1\", \"\").Code = %v, want %v", got, http.StatusOK)
	}
	w := f.do(http.MethodPost, "/group/HEAD?go-get=1", "")
	if got := w.Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("w.Header().Get(\"Allow\") = %v, want %v", got, "GET, HEAD")
	}
}

// gzipRPC sends a service RPC whose body is sent with Content-Encoding: gzip.
func (f *oracleFixture) gzipRPC(t *testing.T, name, service, auth string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/"+name+".git/"+service, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-"+service+"-request")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// A request body that claims gzip but is not answers 400 before any header
// is written, rather than a 200 result followed by an error page. A push
// that sent such a body failed, so a repository it created is removed.
func TestServiceRPCBadGzip(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	_, err := f.be.CreateRepository(ownerCtx, "public", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	w := f.gzipRPC(t, "public", "git-upload-pack", auth, []byte("not gzip"))
	if got := w.Code; got != http.StatusBadRequest {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusBadRequest)
	}
	if strings.Contains(w.Header().Get("Content-Type"), "result") {
		t.Fatal("not true: !strings.Contains(w.Header().Get(\"Content-Type\"), \"result\")")
	}

	if got := f.do(http.MethodGet, "/created.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/created.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	if !f.exists("created") {
		t.Fatal("not true: f.exists(\"created\")")
	}
	w = f.gzipRPC(t, "created", "git-receive-pack", auth, []byte("not gzip"))
	if got := w.Code; got != http.StatusBadRequest {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusBadRequest)
	}
	if f.exists("created") {
		t.Fatal("not true: !f.exists(\"created\")")
	}

	// A well-formed gzip body is still served.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err = zw.Write([]byte("0000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	w = f.gzipRPC(t, "public", "git-upload-pack", auth, buf.Bytes())
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-git-upload-pack-result" {
		t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, "application/x-git-upload-pack-result")
	}
}

// httpGitCounts returns the HTTP fetch and push counters for repo. The
// counters are the process's, and other tests fetch the same names, so the
// tests that read them do not run in parallel.
func httpGitCounts(repo string) (fetches, pushes float64) {
	return testutil.ToFloat64(gitHTTPUploadCounter.WithLabelValues(repo, "info/refs")),
		testutil.ToFloat64(gitHTTPReceiveCounter.WithLabelValues(repo))
}

// Fetches and pushes over HTTP are counted on their ref advertisement only:
// neither the RPCs that follow it nor refused requests are counted.
func TestGitHTTPCounters(t *testing.T) {
	f := newOracleFixture(t)
	const name = "http-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auth := "Token " + token

	rpc := func(repo, service, auth string) int {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/"+repo+".git/"+service, strings.NewReader("0000"))
		req.Header.Set("Content-Type", "application/x-"+service+"-request")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		return w.Code
	}

	fetches, pushes := httpGitCounts(name)
	secretFetches, _ := httpGitCounts("secret")

	// A fetch advertisement and a push advertisement count once each.
	if got := f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", "").Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/\"+name+\".git/info/refs?service=git-upload-pack\", \"\").Code = %v, want %v", got, http.StatusOK)
	}
	if got := f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/\"+name+\".git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	// The RPCs that follow an advertisement belong to the same fetch or
	// push, like the flush-only probe git sends before a large pack.
	if got := rpc(name, "git-upload-pack", ""); got != http.StatusOK {
		t.Fatalf("rpc(name, \"git-upload-pack\", \"\") = %v, want %v", got, http.StatusOK)
	}
	if got := rpc(name, "git-receive-pack", auth); got != http.StatusOK {
		t.Fatalf("rpc(name, \"git-receive-pack\", auth) = %v, want %v", got, http.StatusOK)
	}
	// Refused: an anonymous push, and a fetch of a private repository.
	if f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", "").Code == http.StatusOK {
		t.Fatal("not true: f.do(http.MethodGet, \"/\"+name+\".git/info/refs?service=git-receive-pack\", \"\").Code != http.StatusOK")
	}
	if rpc(name, "git-receive-pack", "") == http.StatusOK {
		t.Fatal("not true: rpc(name, \"git-receive-pack\", \"\") != http.StatusOK")
	}
	if f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", "").Code == http.StatusOK {
		t.Fatal("not true: f.do(http.MethodGet, \"/secret.git/info/refs?service=git-upload-pack\", \"\").Code != http.StatusOK")
	}
	if rpc("secret", "git-upload-pack", "") == http.StatusOK {
		t.Fatal("not true: rpc(\"secret\", \"git-upload-pack\", \"\") != http.StatusOK")
	}

	gotFetches, gotPushes := httpGitCounts(name)
	if got := gotFetches - fetches; got != 1.0 {
		t.Fatalf("gotFetches-fetches = %v, want %v", got, 1.0)
	}
	if got := gotPushes - pushes; got != 1.0 {
		t.Fatalf("gotPushes-pushes = %v, want %v", got, 1.0)
	}
	gotSecret, _ := httpGitCounts("secret")
	if got := gotSecret - secretFetches; got != 0.0 {
		t.Fatalf("gotSecret-secretFetches = %v, want %v", got, 0.0)
	}
}

// A clone or fetch by a real git client is counted once, whichever protocol
// version it speaks, although it sends more than one RPC.
func TestGitHTTPFetchCountedOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newOracleFixture(t)
	const name = "http-fetch-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Give the repository a commit, so a clone fetches a pack. Its hooks run
	// the server binary, which is the test binary here; push with no hooks.
	work := t.TempDir()
	gitRun(t, "init", "-q", work)
	gitRun(t, "-C", work, "commit", "-q", "--allow-empty", "-m", "first")
	bare := filepath.Join(f.env.cfg.DataPath, "repos", name+".git")
	gitRun(t, "-C", work, "-c", "core.hooksPath="+t.TempDir(), "push", "-q", bare, "HEAD:refs/heads/main")

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)
	repoURL := srv.URL + "/" + name + ".git"

	// counted runs git and returns how much it added to the fetch counter.
	counted := func(args ...string) float64 {
		t.Helper()
		before, _ := httpGitCounts(name)
		gitRun(t, args...)
		after, _ := httpGitCounts(name)
		return after - before
	}
	for _, version := range []string{"2", "0"} {
		dst := filepath.Join(t.TempDir(), "clone")
		// A clone sends ls-refs and fetch in protocol v2.
		if got := counted("-c", "protocol.version="+version, "clone", "-q", repoURL, dst); got != 1.0 {
			t.Fatalf("counted(\"-c\", \"protocol.version=\"+version, \"clone\", \"-q\", repoURL, dst) = %v, want %v", got, 1.0)
		}
		// A fetch with nothing new sends no RPC in v0, and ls-refs in v2.
		if got := counted("-C", dst, "-c", "protocol.version="+version, "fetch", "-q", "origin"); got != 1.0 {
			t.Fatalf("counted(\"-C\", dst, \"-c\", \"protocol.version=\"+version, \"fetch\", \"-q\", \"origin\") = %v, want %v", got, 1.0)
		}
	}
}

// A push by a real git client is counted once, even when its pack is larger
// than http.postBuffer and git sends a probe RPC before the real one, and
// even when it has nothing to send and sends no RPC.
func TestGitHTTPPushCountedOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newOracleFixture(t)
	const name = "http-push-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The repository's hooks run the server binary, which is the test binary
	// here, and would refuse the push.
	hooks := filepath.Join(f.env.cfg.DataPath, "repos", name+".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/" + name + ".git")
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("owner", token)

	// A commit with a file that does not compress, larger than the post
	// buffer below.
	work := t.TempDir()
	gitRun(t, "init", "-q", work)
	blob := make([]byte, 256<<10)
	_, err = rand.Read(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "blob"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", work, "add", "blob")
	gitRun(t, "-C", work, "commit", "-q", "-m", "first")

	// counted runs git and returns how much it added to the push counter.
	counted := func(args ...string) float64 {
		t.Helper()
		_, before := httpGitCounts(name)
		gitRun(t, args...)
		_, after := httpGitCounts(name)
		return after - before
	}
	push := []string{"-C", work, "-c", "http.postBuffer=65536", "push", "-q", u.String(), "HEAD:refs/heads/main"}
	if got := counted(push...); got != 1.0 {
		t.Fatalf("counted(push...) = %v, want %v", got, 1.0)
	}
	// Everything is up to date: git reads the advertisement and stops.
	if got := counted(push...); got != 1.0 {
		t.Fatalf("counted(push...) = %v, want %v", got, 1.0)
	}
}

// The go get counter is labelled with the repository a request resolves to,
// not with the path it walked: one readable repository, reached by any
// number of spellings, makes one series. Each label value is a series kept
// for the life of the process, so a label a client controls would let it
// grow the metrics without bound. The fetch counter is held to the same
// rule.
func TestHTTPCountersLabelledByRepository(t *testing.T) {
	f := newOracleFixture(t)
	const name = "label-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	goGets := []string{
		"/" + name + "/x",
		"/" + name + "/x/y",
		"/" + name + ".git/x",
		"/" + name + "/.git/x",
		"/" + name + "/.git/.git/x",
		"/" + name + ".git.git/x",
	}
	// The counters are global, so count the series once the one for name
	// exists: a run before this one (-count=2) may have made it already.
	count := testutil.ToFloat64(goGetCounter.WithLabelValues(name))
	series := testutil.CollectAndCount(goGetCounter)
	for _, p := range goGets {
		w := f.do(http.MethodGet, p+"?go-get=1", "")
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "/"+name+" git ") {
			t.Fatal("not true: strings.Contains(w.Body.String(), \"/\"+name+\" git \")")
		} // resolves to the repository
	}
	if got := testutil.CollectAndCount(goGetCounter) - series; got != 0 {
		t.Fatalf("testutil.CollectAndCount(goGetCounter)-series = %v, want %v", got, 0)
	}
	if got, want := testutil.ToFloat64(goGetCounter.WithLabelValues(name))-count, float64(len(goGets)); got != want {
		t.Fatalf("testutil.ToFloat64(goGetCounter.WithLabelValues(name))-count = %v, want %v", got, want)
	}

	fetches, _ := httpGitCounts(name)
	series = testutil.CollectAndCount(gitHTTPUploadCounter)
	refs := []string{"/" + name, "/" + name + ".git", "/" + name + ".git.git", "/" + name + "/.git"}
	for _, p := range refs {
		if got := f.do(http.MethodGet, p+"/info/refs?service=git-upload-pack", "").Code; got != http.StatusOK {
			t.Fatalf("f.do(http.MethodGet, p+\"/info/refs?service=git-upload-pack\", \"\").Code = %v, want %v", got, http.StatusOK)
		}
	}
	if got := testutil.CollectAndCount(gitHTTPUploadCounter) - series; got != 0 {
		t.Fatalf("testutil.CollectAndCount(gitHTTPUploadCounter)-series = %v, want %v", got, 0)
	}
	gotFetches, _ := httpGitCounts(name)
	if got, want := gotFetches-fetches, float64(len(refs)); got != want {
		t.Fatalf("gotFetches-fetches = %v, want %v", got, want)
	}
}

// countingReader records how many bytes were read from the request body.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The service on the ref advertisement comes from the query string only.
// withParams runs before the method check and authentication, so reading the
// body there let an anonymous multipart POST to any …/info/refs spool its file
// parts to temp files that were never removed.
func TestInfoRefsDoesNotReadBody(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	router := env.gitRouter(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("service", "git-upload-pack"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	body := &countingReader{r: &buf}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/anything/info/refs", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if body.n != 0 {
		t.Errorf("read %d bytes of the request body, want 0", body.n)
	}
}

func TestGetServiceType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, body string
		want               string
	}{
		{"query", "/r/info/refs?service=git-upload-pack", "", "git-upload-pack"},
		{"not a git service", "/r/info/refs?service=upload-pack", "", ""},
		{"body ignored", "/r/info/refs", "service=git-receive-pack", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)) //nolint:noctx
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if got := getServiceType(req).String(); got != tc.want {
				t.Errorf("getServiceType = %q, want %q", got, tc.want)
			}
		})
	}
}

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
	reading  bool  // the body was still being read when serviceRPC returned
	startErr error // the request context's error when the request came in
	endErr   error // the request context's error when serviceRPC returned
}

// newServiceRPCServer serves upload-pack for a new empty repository. Each
// request's flushSignal is sent on flushes, and what the server saw of it
// on results.
func newServiceRPCServer(t *testing.T) (srv *httptest.Server, flushes <-chan *flushSignal, results <-chan handled) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	s := newServer(cfg, nil, nil, nil, nil, nil)

	fc := make(chan *flushSignal, 8)
	rc := make(chan handled, 8)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := handled{startErr: r.Context().Err()}
		body := &trackingBody{ReadCloser: r.Body}
		r.Body = body
		fw := &flushSignal{ResponseWriter: w, flushed: make(chan struct{})}
		fc <- fw
		r = mux.SetURLVars(r, map[string]string{
			"service": "git-upload-pack",
			"dir":     dir,
			"repo":    "repo",
		})
		s.serveRoute((*server).serviceRPC, fw, r, routeCaller{})
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
	case <-time.After(closeWait):
		t.Fatal("timed out waiting for the handler to return")
		return handled{}
	}
}

// checkHandled fails the test if the server read the body after serviceRPC
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

// joiningBody reports whether serviceRPC waits, once git has exited, for the
// copy of the request body to end.
func joiningBody() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, stack, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[chan receive") &&
			strings.Contains(stack, "github.com/charmbracelet/soft-serve/pkg/gitserve.joinStdin(") &&
			strings.Contains(stack, "github.com/charmbracelet/soft-serve/pkg/web.(*server).serviceRPC(") {
			return true
		}
	}
	return false
}

// A client that sends a gzip request git rejects, so git exits, and then
// keeps the request open without sending more must not leave the body being
// read after the handler returns, or the deferred close of the gzip reader
// racing that read. Waiting for it must not cancel the request context, and
// the connection must still serve the next request.
func TestServiceRPCJoinsBodyRead(t *testing.T) {
	t.Parallel()
	srv, flushes, results := newServiceRPCServer(t)

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
	case <-time.After(closeWait):
		t.Fatal("timed out waiting for the request")
	}
	select {
	case <-fw.flushed:
	case h := <-results:
		t.Fatalf("the handler returned while the client was still sending: %+v", h)
	case <-time.After(closeWait):
		t.Fatal("timed out waiting for git to exit")
	}
	// The handler waits for the body once git has exited; until it is
	// there, it must not have returned.
	deadline := time.Now().Add(closeWait)
	for !joiningBody() {
		select {
		case h := <-results:
			t.Fatalf("the handler returned while the client was still sending: %+v", h)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the handler did not wait for the request body")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case h := <-results:
		t.Fatalf("the handler returned while the client was still sending: %+v", h)
	default:
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
func TestServiceRPCKeepAlive(t *testing.T) {
	t.Parallel()
	srv, flushes, results := newServiceRPCServer(t)

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

// Error bodies are plain text that a browser must not sniff into HTML.
func TestErrorBodyIsPlainText(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	for _, path := range []string{"/nope", "/secret.git/info/refs?service=git-upload-pack"} {
		w := f.do(http.MethodGet, path, "")
		if w.Code < 400 {
			t.Fatal("not true: w.Code >= 400")
		}
		if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, "text/plain; charset=utf-8")
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("w.Header().Get(\"X-Content-Type-Options\") = %v, want %v", got, "nosniff")
		}
	}
}

// TestInfoRefsGitFailure covers a ref advertisement that git fails to make
// for a repository that exists. It must answer 500 and log git's reason, not
// 404 as if the repository were missing.
func TestInfoRefsGitFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)

	env := newTestEnvLogging(t, logger)
	ctx, be := env.ctx, env.be
	_, err := be.CreateRepository(ctx, "broken", fixtureAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A HEAD git cannot read makes the directory no longer a repository
	// to git, while soft-serve still knows it.
	dir := filepath.Join(env.cfg.DataPath, "repos", "broken.git")
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/broken.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	env.router(t).ServeHTTP(w, req)

	if got := w.Code; got != http.StatusInternalServerError {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusInternalServerError)
	}
	out := logs.String()
	if !strings.Contains(out, "failed to advertise refs") || !strings.Contains(out, "fatal:") {
		t.Errorf("log does not carry git's reason:\n%s", out)
	}
}

// infoFixture is a router serving a public repository "pub", and the
// repository's directory.
type infoFixture struct {
	handler http.Handler
	dir     string
}

func newInfoFixture(t *testing.T) *infoFixture {
	t.Helper()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be
	_, err := be.CreateRepository(ctx, "pub", fixtureAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(env.cfg.DataPath, "repos", "pub.git")
	return &infoFixture{handler: env.router(t), dir: dir}
}

func (f *infoFixture) get(t *testing.T, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// populate pushes a commit, a lightweight tag, an annotated tag and a tag
// of that tag into the repository, half of it packed.
func (f *infoFixture) populate(t *testing.T) {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "a")
	runGit(t, work, "commit", "-q", "-m", "first")
	runGit(t, work, "tag", "light")
	runGit(t, work, "tag", "-a", "-m", "annotated", "v1")
	runGit(t, work, "tag", "-a", "-m", "nested", "v1-nested", "v1")
	runGit(t, work, "push", "-q", f.dir, "master", "--tags")
	runGit(t, f.dir, "repack", "-q", "-a", "-d")
	if err := os.WriteFile(filepath.Join(work, "b"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "b")
	runGit(t, work, "commit", "-q", "-m", "second")
	runGit(t, work, "push", "-q", f.dir, "master:refs/heads/other")
	runGit(t, f.dir, "repack", "-q", "-d")
}

// TestSmartInfoRefs checks the streamed smart advertisement keeps its
// header, the service line before protocol v2, and git's output.
func TestSmartInfoRefs(t *testing.T) {
	t.Parallel()
	f := newInfoFixture(t)
	f.populate(t)
	head := strings.Fields(runGit(t, f.dir, "rev-parse", "refs/heads/master"))[0]

	w := f.get(t, "/pub.git/info/refs?service=git-upload-pack")
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("w.Header().Get(\"Content-Type\") = %v, want %v", got, "application/x-git-upload-pack-advertisement")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache, max-age=0, must-revalidate" {
		t.Fatalf("w.Header().Get(\"Cache-Control\") = %v, want %v", got, "no-cache, max-age=0, must-revalidate")
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "001e# service=git-upload-pack\n0000") {
		t.Fatal("not true: strings.HasPrefix(body, \"001e# service=git-upload-pack\\n0000\")")
	}
	if !strings.Contains(body, head+" refs/heads/master") {
		t.Fatal("not true: strings.Contains(body, head+\" refs/heads/master\")")
	}
	if !strings.HasSuffix(body, "0000") {
		t.Fatal("not true: strings.HasSuffix(body, \"0000\")")
	}

	w = f.get(t, "/pub.git/info/refs?service=git-upload-pack", "Git-Protocol", "version=2")
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if !strings.HasPrefix(w.Body.String(), "000eversion 2\n") {
		t.Fatal("not true: strings.HasPrefix(w.Body.String(), \"000eversion 2\\n\")")
	}
}

// TestInfoRefsServiceLineFollowsServedVersion checks the service line is
// there exactly when git serves a version before v2, which it reads from
// Git-Protocol as hooks.ProtocolVersion does: the highest of 0, 1 and 2,
// ignoring the rest.
func TestInfoRefsServiceLineFollowsServedVersion(t *testing.T) {
	t.Parallel()
	f := newInfoFixture(t)
	f.populate(t)
	const serviceLine = "001e# service=git-upload-pack\n0000"
	for _, tc := range []struct {
		protocol, want string
	}{
		{"version=3", serviceLine},
		{"version=02", serviceLine},
		{"version=1:version=3", serviceLine + "000eversion 1\n"},
		{"version=3:version=2", "000eversion 2\n"},
	} {
		w := f.get(t, "/pub.git/info/refs?service=git-upload-pack", "Git-Protocol", tc.protocol)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), tc.want) {
			t.Errorf("Git-Protocol %q: %d %q, want 200 starting with %q", tc.protocol, w.Code, w.Body.String()[:min(w.Body.Len(), 40)], tc.want)
		}
	}
}

// TestAdvertisementStartsOnFirstWrite checks the header is held back until
// git writes, so a git that fails silently still gets an error status.
func TestAdvertisementStartsOnFirstWrite(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	starts := 0
	a := &advertisement{w: &out, start: func() { starts++; out.WriteString("hdr|") }}

	n, err := a.Write(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := n; got != 0 {
		t.Fatalf("n = %v, want %v", got, 0)
	}
	if got := starts; got != 0 {
		t.Fatalf("starts = %v, want %v", got, 0)
	}
	if a.started {
		t.Fatal("not true: !a.started")
	}

	_, err = a.Write([]byte("one|"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Write([]byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	a.begin()
	if got := starts; got != 1 {
		t.Fatalf("starts = %v, want %v", got, 1)
	}
	if got := out.String(); got != "hdr|one|two" {
		t.Fatalf("out.String() = %v, want %v", got, "hdr|one|two")
	}
}

// duplexRecorder records whether the handler asked for full duplex.
type duplexRecorder struct {
	*httptest.ResponseRecorder
	fullDuplex bool
}

func (d *duplexRecorder) EnableFullDuplex() error {
	d.fullDuplex = true
	return nil
}

// TestServiceRPCEnablesFullDuplex checks the RPC handler lets git read the
// request while it writes the response.
func TestServiceRPCEnablesFullDuplex(t *testing.T) {
	t.Parallel()
	f := newInfoFixture(t)
	f.populate(t)

	// A protocol v2 ls-refs request, which needs no negotiation.
	body := "0014command=ls-refs\n0000"
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pub.git/git-upload-pack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	w := &duplexRecorder{ResponseRecorder: httptest.NewRecorder()}
	f.handler.ServeHTTP(w, req)

	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	if !w.fullDuplex {
		t.Fatal("not true: w.fullDuplex")
	}
	if !strings.Contains(w.Body.String(), "refs/heads/master") {
		t.Fatal("not true: strings.Contains(w.Body.String(), \"refs/heads/master\")")
	}
}

// With allow-keyless off, an anonymous caller is asked for credentials on
// the git routes and on go get, even for a public repository, and a caller
// with a token is served.
func TestKeylessDisabledRequiresCaller(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.be.SetAllowKeyless(f.ctx, false); err != nil {
		t.Fatal(err)
	}

	auth := "Token " + f.token
	for _, path := range []string{"/pub.git/info/refs?service=git-upload-pack", "/pub?go-get=1"} {
		w := f.do(http.MethodGet, path, "")
		if got := w.Code; got != http.StatusUnauthorized {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusUnauthorized)
		} // anonymous
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("not true: w.Header().Get(\"WWW-Authenticate\") != \"\"")
		}
		if got := f.do(http.MethodGet, path, auth).Code; got != http.StatusOK {
			t.Fatalf("f.do(http.MethodGet, path, auth).Code = %v, want %v", got, http.StatusOK)
		} // with a token
	}
}

// TestHTTPMirrorRefusesPush verifies that a caller allowed to write gets a
// 403 carrying ErrMirrorReadOnly for both the receive-pack advertisement and
// the receive-pack RPC on a mirror, while upload-pack still serves it, and
// that turning mirroring off lets the push advertisement through again.
func TestHTTPMirrorRefusesPush(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	_, err := f.be.CreateRepository(ownerCtx, "m", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.be.AddCollaborator(ownerCtx, "m", "stranger", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	r, err := git.Open(t.Context(), filepath.Join(f.env.cfg.DataPath, "repos", "m.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("remote.origin.url", "https://1.1.1.1/x.git"); err != nil {
		t.Fatal(err)
	}
	if err := f.be.SetMirror(ownerCtx, "m", true); err != nil {
		t.Fatal(err)
	}

	want := proto.ErrMirrorReadOnly.Error()
	adv := f.do(http.MethodGet, "/m.git/info/refs?service=git-receive-pack", auth)
	if got := adv.Code; got != http.StatusForbidden {
		t.Fatalf("adv.Code = %v, want %v", got, http.StatusForbidden)
	}
	if got := strings.TrimSpace(adv.Body.String()); got != want {
		t.Fatalf("strings.TrimSpace(adv.Body.String()) = %v, want %v", got, want)
	}

	rpc := f.do(http.MethodPost, "/m.git/git-receive-pack", auth)
	if got := rpc.Code; got != http.StatusForbidden {
		t.Fatalf("rpc.Code = %v, want %v", got, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rpc.Body.String()); got != want {
		t.Fatalf("strings.TrimSpace(rpc.Body.String()) = %v, want %v", got, want)
	}

	// Reads are not refused.
	up := f.do(http.MethodGet, "/m.git/info/refs?service=git-upload-pack", auth)
	if got := up.Code; got != http.StatusOK {
		t.Fatalf("up.Code = %v, want %v", got, http.StatusOK)
	}
	if strings.Contains(up.Body.String(), want) {
		t.Fatal("not true: !strings.Contains(up.Body.String(), want)")
	}

	// Without mirroring the push advertisement is served.
	if err := f.be.SetMirror(ownerCtx, "m", false); err != nil {
		t.Fatal(err)
	}
	adv = f.do(http.MethodGet, "/m.git/info/refs?service=git-receive-pack", auth)
	if got := adv.Code; got != http.StatusOK {
		t.Fatalf("adv.Code = %v, want %v", got, http.StatusOK)
	}
}

// oracleFixture wires a router with a private repository owned by "owner"
// and a token for "stranger", a registered user with no access to it.
type oracleFixture struct {
	env     *testEnv
	ctx     context.Context
	owner   proto.User
	be      *backend.Backend
	handler http.Handler
	token   string
	// server serves handler.
	server *server
	// pending is the list of push-created repositories handler keeps.
	pending *pushCreates
}

// newOracleFixture returns a fixture whose server logs on log.Default().
func newOracleFixture(t *testing.T) *oracleFixture {
	t.Helper()
	return newOracleFixtureLogging(t, nil)
}

// newOracleFixtureLogging returns a fixture whose server logs on logger.
func newOracleFixtureLogging(t *testing.T, logger *log.Logger) *oracleFixture {
	t.Helper()
	env := newTestEnvLogging(t, logger)
	ctx, be := env.ctx, env.be

	// Persist a host key so tokens minted here verify against the same key
	// the server uses.
	env.writeHostKey(t)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, stranger, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	s := env.server(t)
	return &oracleFixture{env: env, ctx: ctx, owner: owner, be: be, handler: s.router(), token: token, server: s, pending: s.pending}
}

// do performs a request and returns the recorded response. An empty auth
// header means an anonymous request.
func (f *oracleFixture) do(method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// answer is the part of a response a caller can compare between two
// repository names: status, the credentials challenge, and the body.
type answer struct {
	code    int
	wwwAuth string
	body    string
}

func answerOf(w *httptest.ResponseRecorder) answer {
	return answer{
		code:    w.Code,
		wwwAuth: w.Header().Get("WWW-Authenticate"),
		body:    w.Body.String(),
	}
}

// assertSameAnswer requests the same path for the private repository and
// for a missing one and checks the caller cannot tell them apart.
func (f *oracleFixture) assertSameAnswer(t *testing.T, method, path, auth string, wantCode int) {
	t.Helper()
	private := answerOf(f.do(method, fmt.Sprintf(path, "secret"), auth))
	missing := answerOf(f.do(method, fmt.Sprintf(path, "ghost"), auth))
	if got := private.code; got != wantCode {
		t.Fatalf("private.code = %v, want %v", got, wantCode)
	}
	if got, want := missing, private; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
}

// TestReceivePackDoesNotRevealPrivateRepositories covers the push paths,
// which must answer a private repository and a missing one alike to the
// same signed-in user.
func TestReceivePackDoesNotRevealPrivateRepositories(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token

	// Former dumb HTTP routes that carry ?service=git-receive-pack never
	// create a repository, and answer the same for private and missing.
	for _, path := range []string{
		"/%s.git/HEAD?service=git-receive-pack",
		"/%s.git/objects/info/packs?service=git-receive-pack",
	} {
		f.assertSameAnswer(t, http.MethodGet, path, auth, http.StatusForbidden)
		f.assertSameAnswer(t, http.MethodGet, path, "", http.StatusForbidden)
	}

	// The push advertisement and RPC answer the same for an anonymous
	// caller, who cannot create a repository.
	f.assertSameAnswer(t, http.MethodGet, "/%s.git/info/refs?service=git-receive-pack", "", http.StatusUnauthorized)
	f.assertSameAnswer(t, http.MethodPost, "/%s.git/git-receive-pack", "", http.StatusUnauthorized)

	// Nothing above created a repository.
	_, err := f.be.Repository(f.ctx, "ghost")
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	// A signed-in user's push to a private repository they cannot read is
	// a 404, the same as every other unreadable repository, while a push
	// to a new name still creates it.
	if got := f.do(http.MethodGet, "/secret.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusNotFound {
		t.Fatalf("f.do(http.MethodGet, \"/secret.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusNotFound)
	}
	if got := f.do(http.MethodPost, "/secret.git/git-receive-pack", auth).Code; got != http.StatusNotFound {
		t.Fatalf("f.do(http.MethodPost, \"/secret.git/git-receive-pack\", auth).Code = %v, want %v", got, http.StatusNotFound)
	}
	if got := f.do(http.MethodGet, "/ghost.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/ghost.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	_, err = f.be.Repository(f.ctx, "ghost")
	if err != nil {
		t.Fatal(err)
	}

	// With admin-only creation on, a non-admin's push to a new name is
	// refused like any other unreadable repository and creates nothing.
	f.env.cfg.AdminOnlyCreate = true
	if got := f.do(http.MethodGet, "/ghost2.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusNotFound {
		t.Fatalf("f.do(http.MethodGet, \"/ghost2.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusNotFound)
	}
	if got := f.do(http.MethodPost, "/ghost2.git/git-receive-pack", auth).Code; got != http.StatusNotFound {
		t.Fatalf("f.do(http.MethodPost, \"/ghost2.git/git-receive-pack\", auth).Code = %v, want %v", got, http.StatusNotFound)
	}
	_, err = f.be.Repository(f.ctx, "ghost2")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// TestReceivePackReaderIsForbidden verifies a caller who can read but not
// write is told so rather than being treated as unable to see the repo.
func TestReceivePackReaderIsForbidden(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	if err := f.be.AddCollaborator(ownerCtx, "secret", "stranger", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}

	auth := "Token " + f.token
	if got := f.do(http.MethodGet, "/secret.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/secret.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusForbidden)
	}
	if got := f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/secret.git/info/refs?service=git-upload-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}

	// The service parameter does not turn a read into a push off the
	// advertisement.
	if got := f.do(http.MethodGet, "/secret.git/HEAD?service=git-receive-pack", auth).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, \"/secret.git/HEAD?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusForbidden)
	}
	want := answerOf(f.do(http.MethodGet, "/secret.git/objects/info/whatever", auth))
	if got := want.code; got != http.StatusNotFound {
		t.Fatalf("want.code = %v, want %v", got, http.StatusNotFound)
	}
	if got, want := answerOf(f.do(http.MethodGet, "/secret.git/objects/info/whatever?service=git-receive-pack", auth)), want; !reflect.DeepEqual(got, want) {
		t.Fatalf("answerOf(f.do(http.MethodGet, \"/secret.git/objects/info/whatever?service=git-receive-pack\", auth)) = %v, want %v", got, want)
	}

	// A GET whose path ends in the push service name is routed to the RPC
	// handler with a nested repository name. The method is refused
	// before anything else, for any caller: authorized as a push, it
	// could create that nested repository.
	for _, name := range []string{"secret", "ghost"} {
		for _, a := range []string{auth, ""} {
			got := answerOf(f.do(http.MethodGet, "/"+name+"/objects/info/git-receive-pack", a))
			if got := got.code; got != http.StatusMethodNotAllowed {
				t.Fatalf("got.code = %v, want %v", got, http.StatusMethodNotAllowed)
			}
		}
	}
	_, err := f.be.Repository(f.ctx, "secret/objects/info")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = f.be.Repository(f.ctx, "ghost/objects/info")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// mintJWT signs a token for user the way `soft jwt` does, with f's host
// key.
func (f *oracleFixture) mintJWT(t *testing.T, user proto.User, audience ...string) string {
	t.Helper()
	return f.env.mintJWT(t, user, audience...)
}

// TestBearerTokenDoesNotRevealPrivateRepositories covers JWT auth: a token
// for a missing repository is checked as one for a private repository is,
// so the answers do not tell the two apart.
func TestBearerTokenDoesNotRevealPrivateRepositories(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	stranger, err := f.be.User(f.ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}

	const path = "/%s.git/info/refs?service=git-upload-pack"
	mint := func(audience ...string) string {
		return "Bearer " + f.mintJWT(t, stranger, audience...)
	}

	// A token whose audience names the requested repository identifies the
	// user whether or not the repository exists.
	for _, name := range []string{"secret", "ghost"} {
		if got := answerOf(f.do(http.MethodGet, fmt.Sprintf(path, name), mint(name))).code; got != http.StatusNotFound {
			t.Fatalf("answerOf(f.do(http.MethodGet, fmt.Sprintf(path, name), mint(name))).code = %v, want %v", got, http.StatusNotFound)
		}
	}
	private := answerOf(f.do(http.MethodGet, fmt.Sprintf(path, "secret"), mint("secret")))
	missing := answerOf(f.do(http.MethodGet, fmt.Sprintf(path, "ghost"), mint("ghost")))
	if got, want := missing, private; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}

	// The same holds when the token names several repositories.
	private = answerOf(f.do(http.MethodGet, fmt.Sprintf(path, "secret"), mint("a", "secret", "b")))
	missing = answerOf(f.do(http.MethodGet, fmt.Sprintf(path, "ghost"), mint("a", "ghost", "b")))
	if got := private.code; got != http.StatusNotFound {
		t.Fatalf("private.code = %v, want %v", got, http.StatusNotFound)
	}
	if got, want := missing, private; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}

	// A token minted for some other repository, or for none, is bad
	// credentials either way.
	for _, audience := range [][]string{{"elsewhere"}, {"a", "b"}, {}} {
		f.assertSameAnswer(t, http.MethodGet, path, mint(audience...), http.StatusForbidden)
	}

	// A valid token still grants the user their own access.
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	if err := f.be.AddCollaborator(ownerCtx, "secret", "stranger", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if got := f.do(http.MethodGet, fmt.Sprintf(path, "secret"), mint("secret")).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, fmt.Sprintf(path, \"secret\"), mint(\"secret\")).Code = %v, want %v", got, http.StatusOK)
	}

	// The audience is matched against the sanitized name, which is what
	// `soft jwt` mints, so a ".git" suffix in the token does not match.
	if got := f.do(http.MethodGet, fmt.Sprintf(path, "secret"), mint("secret.git")).Code; got != http.StatusForbidden {
		t.Fatalf("f.do(http.MethodGet, fmt.Sprintf(path, \"secret\"), mint(\"secret.git\")).Code = %v, want %v", got, http.StatusForbidden)
	}
}

// commitInRepo writes a commit into the bare repository at dir, without a
// ref, and returns its ID.
func commitInRepo(t *testing.T, dir string) string {
	t.Helper()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := run("hello\n", "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tREADME.md\n", "mktree")
	return run("", "commit-tree", tree, "-m", "first")
}

// A push whose client goes away after receive-pack updated its refs still
// gets the work done after the push: git ran post-receive, which wrote the
// refs file, and the service then failed with the cancellation. The refs
// are read, the backend writes info/last-modified, and the file is removed.
// Here a stand-in for git updates the ref, writes the refs file as the
// post-receive hook does, and returns context.Canceled.
func TestPushAfterClientWentAwayStillRunsAfterPush(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	// The fixture's token is the stranger's, so the stranger owns it.
	stranger, err := f.be.User(f.ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.be.CreateRepository(proto.WithUserContext(f.ctx, stranger), "public", stranger, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.env.cfg
	dir := hooks.RepoPath(cfg, "public")
	commit := commitInRepo(t, dir)
	lastModified := filepath.Join(dir, "info", "last-modified")
	_, err = os.Stat(lastModified)
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}

	var refsFile string
	orig := f.server.runService
	f.server.runService = func(ctx context.Context, service gitserve.Service, cmd gitserve.ServiceCommand) error {
		if service != gitserve.ReceivePackService {
			return orig(ctx, service, cmd)
		}
		for _, kv := range cmd.Env {
			if v, ok := strings.CutPrefix(kv, hooks.PushRefsEnv+"="); ok {
				refsFile = v
			}
		}
		if refsFile == "" {
			t.Error("receive-pack got no refs file")
			return context.Canceled
		}
		ref := exec.CommandContext(t.Context(), "git", "--git-dir", dir, "update-ref", "refs/heads/main", commit)
		ref.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if err := ref.Run(); err != nil {
			t.Errorf("update-ref: %v", err)
		}
		line := strings.Repeat("0", 40) + " " + commit + " refs/heads/main\n"
		if err := os.WriteFile(refsFile, []byte(line), 0o600); err != nil {
			t.Error(err)
		}
		return context.Canceled
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/public.git/git-receive-pack", bytes.NewReader([]byte("0000")))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Authorization", "Token "+f.token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	if refsFile == "" {
		t.Fatal("not true: refsFile != \"\"")
	}
	// AfterPush ran: it wrote last-modified before the handler returned.
	_, err = os.Stat(lastModified)
	if err != nil {
		t.Fatal(err)
	}
	// And the refs file is gone.
	_, err = os.Stat(refsFile)
	if !os.IsNotExist(err) {
		t.Fatal("not true: os.IsNotExist(err)")
	}
}

// routeNeed is what a git or LFS route asks of a caller for one method.
type routeNeed int

const (
	// notAllowed: the method is refused with 405 before anything else.
	notAllowed routeNeed = iota
	// needRead: a caller who may read the repository is served.
	needRead
	// needWrite: a caller who may only read is refused.
	needWrite
	// preflight: answered by the CORS middleware before any route, the
	// same for every caller.
	preflight
)

func (n routeNeed) String() string {
	return [...]string{"405", "read", "write", "cors"}[n]
}

// routeWant is the classification of one route and method.
type routeWant struct {
	need routeNeed
	// lfs routes answer refusals with an LFS JSON body, others with text.
	lfs bool
	// creates is whether a caller who may create repositories creates a
	// missing one by this request (push-to-create).
	creates bool
}

// TestRouteAccessTable pins, by requests through the router, the access
// level every git and LFS route needs for every method, whether its refusals
// are LFS JSON, and which requests create a missing repository. It is a
// black-box test on purpose: it describes the behaviour, not the code that
// decides it, so it holds however the route table is written.
func TestRouteAccessTable(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)

	// "pub" is public: anonymous callers and "stranger" may read it, and
	// only its owner may write. "secret" is private to its owner.
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	if err := f.be.AddCollaborator(ownerCtx, "pub", "stranger", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	writer := f.ownerAuth(t)
	reader := "Token " + f.token
	// A user who may create repositories, and can read nothing of the
	// owner's.
	creator, err := f.be.CreateUser(f.ctx, "creator", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	creatorToken, err := f.be.CreateAccessToken(f.ctx, creator, "c", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	oid := strings.Repeat("a", 64)
	read := routeWant{need: needRead}
	write := routeWant{need: needWrite}
	lfsRead := routeWant{need: needRead, lfs: true}
	lfsWrite := routeWant{need: needWrite, lfs: true}
	push := routeWant{need: needWrite, creates: true}
	no := routeWant{need: notAllowed}
	noLFS := routeWant{need: notAllowed, lfs: true}

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	routes := []struct {
		path string // under /<repo>.git
		want map[string]routeWant
	}{
		{"/git-upload-pack", map[string]routeWant{http.MethodPost: read}},
		{"/git-upload-archive", map[string]routeWant{http.MethodPost: read}},
		{"/git-receive-pack", map[string]routeWant{http.MethodPost: push}},
		{"/info/refs?service=git-upload-pack", map[string]routeWant{http.MethodGet: read, http.MethodHead: read}},
		// HEAD answers like GET but does not create the repository.
		{"/info/refs?service=git-receive-pack", map[string]routeWant{http.MethodGet: push, http.MethodHead: write}},
		{"/info/lfs/objects/batch", map[string]routeWant{http.MethodPost: lfsRead}},
		{"/info/lfs/objects/basic/" + oid, map[string]routeWant{http.MethodGet: lfsRead, http.MethodHead: lfsRead, http.MethodPut: lfsWrite}},
		{"/info/lfs/objects/basic/verify", map[string]routeWant{http.MethodPost: lfsRead}},
		{"/info/lfs/locks", map[string]routeWant{http.MethodGet: lfsRead, http.MethodHead: lfsRead, http.MethodPost: lfsWrite}},
		{"/info/lfs/locks/verify", map[string]routeWant{http.MethodPost: lfsWrite}},
		{"/info/lfs/locks/1/unlock", map[string]routeWant{http.MethodPost: lfsWrite}},
	}

	// denied reports whether the authorization middleware refused the
	// request. Handlers answer the empty requests sent here with 2xx, 404,
	// 415 or 422, never 401 or 403.
	denied := func(code int) bool {
		return code == http.StatusUnauthorized || code == http.StatusForbidden
	}

	fresh := 0
	for _, rt := range routes {
		for _, method := range methods {
			want, ok := rt.want[method]
			if !ok {
				want = no
				if strings.HasPrefix(rt.path, "/info/lfs") {
					want = noLFS
				}
			}
			if method == http.MethodOptions {
				want = routeWant{need: preflight, lfs: want.lfs}
			}
			name := method + " " + rt.path
			at := func(repo string) string { return "/" + repo + ".git" + rt.path }

			var got routeWant
			got.lfs = want.lfs // checked below where a refusal has a body
			switch w := f.do(method, at("pub"), writer); {
			case method == http.MethodOptions && w.Code == http.StatusOK && w.Body.Len() == 0:
				got.need = preflight
				for _, auth := range []string{"", reader} {
					if a := f.do(method, at("secret"), auth); a.Code != http.StatusOK || a.Body.Len() != 0 {
						t.Errorf("%s: 200 for the owner, %d %q for another caller", name, a.Code, a.Body)
					}
				}
			case w.Code == http.StatusMethodNotAllowed:
				got.need = notAllowed
				// Every caller gets the same 405.
				for _, auth := range []string{"", reader} {
					if c := f.do(method, at("secret"), auth).Code; c != http.StatusMethodNotAllowed {
						t.Errorf("%s: 405 for the owner, %d for another caller", name, c)
					}
				}
			case denied(w.Code):
				t.Errorf("%s: the owner was refused with %d", name, w.Code)
				continue
			default:
				if denied(f.do(method, at("pub"), reader).Code) {
					got.need = needWrite
					// An anonymous caller is asked for credentials.
					a := f.do(method, at("pub"), "")
					if a.Code != http.StatusUnauthorized || a.Header().Get("WWW-Authenticate") == "" {
						t.Errorf("%s: anonymous reader got %d, want 401 with a challenge", name, a.Code)
					}
				} else {
					got.need = needRead
					if c := f.do(method, at("pub"), "").Code; denied(c) {
						t.Errorf("%s: anonymous reader of a public repo refused with %d", name, c)
					}
				}
				// Unreadable: refused, as JSON on LFS routes.
				a := f.do(method, at("secret"), "")
				if a.Code != http.StatusUnauthorized {
					t.Errorf("%s: anonymous on a private repo got %d, want 401", name, a.Code)
				}
				got.lfs = strings.HasPrefix(a.Header().Get("Content-Type"), "application/vnd.git-lfs+json")
				if !got.lfs && !strings.HasPrefix(a.Header().Get("Content-Type"), "text/plain") {
					t.Errorf("%s: refusal has Content-Type %q", name, a.Header().Get("Content-Type"))
				}
			}

			// Push-to-create: only a creator's push requests create.
			fresh++
			repo := fmt.Sprintf("fresh%d", fresh)
			f.do(method, at(repo), "Token "+creatorToken)
			_, err := f.be.Repository(f.ctx, repo)
			got.creates = err == nil

			if got != want {
				t.Errorf("%s: got {%v lfs=%v creates=%v}, want {%v lfs=%v creates=%v}",
					name, got.need, got.lfs, got.creates, want.need, want.lfs, want.creates)
			}
		}
	}
}

// TestRouteAccessAnonymousWriter pins what an anonymous caller gets on the
// write routes of a public repository when anonymous users may write
// (anon-access read-write): authorization lets every request through, so
// the answer is the handler's. Under anon-access read-only the same
// requests are refused by authorization with 401. Creating or deleting a
// lock then asks for credentials in the handler, since a lock needs an
// owner.
func TestRouteAccessAnonymousWriter(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	pub, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A lock of the owner's, which verify lists for others as theirs.
	_, err = f.env.datastore.CreateLFSLockForUser(f.ctx, f.env.dbx, pub.ID(), f.owner.ID(), "owned.bin", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	oid := sha256Hex("x")
	lfsJSON := map[string]string{"Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	octets := map[string]string{"Content-Type": "application/octet-stream"}
	rpc := map[string]string{"Content-Type": "application/x-git-receive-pack-request"}
	rows := []struct {
		method, path string
		hdr          map[string]string
		body         string
		// want is the status with anon-access read-write.
		want int
		// challenge is whether that answer asks for credentials.
		challenge bool
	}{
		{http.MethodGet, "/info/refs?service=git-receive-pack", nil, "", http.StatusOK, false},
		{http.MethodHead, "/info/refs?service=git-receive-pack", nil, "", http.StatusOK, false},
		{http.MethodPost, "/git-receive-pack", nil, "", http.StatusUnsupportedMediaType, false},
		{http.MethodPost, "/git-receive-pack", rpc, "0000", http.StatusOK, false},
		{http.MethodPut, "/info/lfs/objects/basic/" + oid, nil, "x", http.StatusUnsupportedMediaType, false},
		{http.MethodPut, "/info/lfs/objects/basic/" + oid, octets, "x", http.StatusOK, false},
		{http.MethodPost, "/info/lfs/locks", lfsJSON, "{", http.StatusBadRequest, false},
		{http.MethodPost, "/info/lfs/locks", lfsJSON, `{"path":"a.bin"}`, http.StatusUnauthorized, true},
		// Verify answers; see the body check below.
		{http.MethodPost, "/info/lfs/locks/verify", lfsJSON, `{}`, http.StatusOK, false},
		{http.MethodPost, "/info/lfs/locks/1/unlock", lfsJSON, `{}`, http.StatusUnauthorized, true},
	}

	do := func(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		return f.send(method, "/pub.git"+path, hdr, body)
	}

	// anon-access read-only: authorization refuses each, asking for
	// credentials.
	if err := f.be.SetAnonAccess(f.ctx, access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		w := do(row.method, row.path, row.hdr, row.body)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("read-only: %s %s: got %d, want 401 with a challenge", row.method, row.path, w.Code)
		}
	}

	if err := f.be.SetAnonAccess(f.ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		w := do(row.method, row.path, row.hdr, row.body)
		if w.Code != row.want || (w.Header().Get("WWW-Authenticate") != "") != row.challenge {
			t.Errorf("read-write: %s %s %q: got %d (challenge %q), want %d (challenge %v): %s",
				row.method, row.path, row.body, w.Code, w.Header().Get("WWW-Authenticate"), row.want, row.challenge, w.Body)
		}
	}

	// The anonymous writer owns no lock: the owner's comes back as theirs.
	w := do(http.MethodPost, "/info/lfs/locks/verify", lfsJSON, `{}`)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}
	var verify lfs.LockVerifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &verify); err != nil {
		t.Fatal(err)
	}
	if got := len(verify.Ours); got != 0 {
		t.Fatalf("len(verify.Ours) = %v, want %v", got, 0)
	}
	if got := len(verify.Theirs); got != 1 {
		t.Fatalf("len(verify.Theirs) = %v, want %v", got, 1)
	}
	if got := verify.Theirs[0].Path; got != "owned.bin" {
		t.Fatalf("verify.Theirs[0].Path = %v, want %v", got, "owned.bin")
	}
	if got, want := verify.Theirs[0].Owner.Name, f.owner.Username(); got != want {
		t.Fatalf("verify.Theirs[0].Owner.Name = %v, want %v", got, want)
	}
}
