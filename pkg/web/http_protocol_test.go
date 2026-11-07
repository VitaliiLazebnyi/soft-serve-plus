package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
	"github.com/matryer/is"
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
	is := is.New(t)
	f := newOracleFixture(t)
	w := f.do(http.MethodGet, "/secret.git/info/refs", "")
	is.Equal(w.Code, http.StatusUnauthorized)
	is.Equal(w.Header().Get("WWW-Authenticate"), `Basic realm="Git", charset="UTF-8", Token, Bearer`)
	is.Equal(w.Header().Get("LFS-Authenticate"), `Basic realm="Git LFS", charset="UTF-8", Token, Bearer`)
}

// A wrong method gets 405 with an Allow header whatever the protocol
// version, and a route that answers GET answers HEAD too.
func TestMethodNotAllowed(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)

	for _, proto := range []struct {
		name         string
		major, minor int
	}{{"HTTP/1.0", 1, 0}, {"HTTP/1.1", 1, 1}, {"HTTP/2.0", 2, 0}} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/secret.git/info/lfs/objects/batch", nil)
		req.Proto, req.ProtoMajor, req.ProtoMinor = proto.name, proto.major, proto.minor
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		is.Equal(w.Code, http.StatusMethodNotAllowed)
		is.Equal(w.Header().Get("Allow"), http.MethodPost)
	}

	w := f.do(http.MethodPost, "/secret.git/info/refs", auth)
	is.Equal(w.Code, http.StatusMethodNotAllowed)
	is.Equal(w.Header().Get("Allow"), "GET, HEAD")

	is.Equal(f.do(http.MethodHead, "/secret.git/info/refs?service=git-upload-pack", auth).Code, http.StatusOK)
	is.Equal(f.do(http.MethodHead, "/secret.git/HEAD", auth).Code, http.StatusOK)

	// HEAD on the push advertisement of a missing repository does not
	// create it, though GET would for a caller who may push.
	is.Equal(f.do(http.MethodHead, "/fresh.git/info/refs?service=git-receive-pack", "Token "+f.token).Code, http.StatusNotFound)
	is.True(!f.exists("fresh"))
}

// The RPC endpoints answer 415 to any body but the service's request type,
// as git http-backend does, and their responses are not cached.
func TestServiceRPCContentType(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)

	for _, ct := range []string{"", "text/plain", "application/x-git-upload-pack-requestx", "application/x-git-receive-pack-request"} {
		w := f.send(http.MethodPost, "/secret.git/git-upload-pack", map[string]string{"Authorization": auth, "Content-Type": ct}, "")
		is.Equal(w.Code, http.StatusUnsupportedMediaType)
	}

	for _, ct := range []string{"application/x-git-upload-pack-request", "application/x-git-upload-pack-request; charset=utf-8"} {
		w := f.send(http.MethodPost, "/secret.git/git-upload-pack", map[string]string{"Authorization": auth, "Content-Type": ct}, "0000")
		is.Equal(w.Code, http.StatusOK)
		is.Equal(w.Header().Get("Content-Type"), "application/x-git-upload-pack-result")
		is.Equal(w.Header().Get("Cache-Control"), "no-cache, max-age=0, must-revalidate")
	}
}

// signJWT signs claims with the server key.
func signJWT(ctx context.Context, t *testing.T, claims jwt.RegisteredClaims) string {
	t.Helper()
	kp, err := jwk.NewPair(config.FromContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwk.SigningMethod, claims)
	token.Header["kid"] = kp.JWK().KeyID
	signed, err := token.SignedString(kp.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// A bearer token must expire.
func TestBearerTokenRequiresExpiry(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:  fmt.Sprintf("%s#%d", f.owner.Username(), f.owner.ID()),
		IssuedAt: jwt.NewNumericDate(now),
		Issuer:   config.FromContext(f.ctx).HTTP.PublicURL,
		Audience: []string{"secret"},
	}
	const path = "/secret.git/info/refs?service=git-upload-pack"
	is.Equal(f.do(http.MethodGet, path, "Bearer "+signJWT(f.ctx, t, claims)).Code, http.StatusForbidden)

	claims.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour))
	is.Equal(f.do(http.MethodGet, path, "Bearer "+signJWT(f.ctx, t, claims)).Code, http.StatusOK)
}

// go get of a package inside a repository accepts a token minted for the
// repository, but not one minted for another repository.
func TestGoGetBearerTokenForParentRepository(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)

	for _, path := range []string{"/secret?go-get=1", "/secret/sub/pkg?go-get=1", "/secret.git/sub?go-get=1"} {
		w := f.do(http.MethodGet, path, "Bearer "+mintJWT(f.ctx, t, f.owner, "secret"))
		is.Equal(w.Code, http.StatusOK)
		is.True(strings.Contains(w.Body.String(), `name="go-import"`))
		is.True(strings.Contains(w.Body.String(), "https://pkg.go.dev/"))
	}
	is.Equal(f.do(http.MethodGet, "/secret/sub?go-get=1", "Bearer "+mintJWT(f.ctx, t, f.owner, "other")).Code, http.StatusForbidden)
	is.Equal(f.do(http.MethodGet, "/secret/sub?go-get=1", "Bearer "+mintJWT(f.ctx, t, f.owner, "secret/sub/pkg")).Code, http.StatusForbidden)

	// A token for "secret" does not reveal the nested repository
	// "secret/sub", even to a caller who may read it: the walk skips it
	// and answers for "secret".
	_, err := f.be.CreateRepository(f.ctx, "secret/sub", f.owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	for _, path := range []string{"/secret/sub?go-get=1", "/secret/sub/pkg?go-get=1"} {
		w := f.do(http.MethodGet, path, "Bearer "+mintJWT(f.ctx, t, f.owner, "secret"))
		is.Equal(w.Code, http.StatusOK)
		is.True(strings.Contains(w.Body.String(), "/secret.git"))
		is.True(!strings.Contains(w.Body.String(), "secret/sub"))

		w = f.do(http.MethodGet, path, "Bearer "+mintJWT(f.ctx, t, f.owner, "secret/sub"))
		is.Equal(w.Code, http.StatusOK)
		is.True(strings.Contains(w.Body.String(), "/secret/sub.git"))
	}
}

func TestPathAncestors(t *testing.T) {
	is := is.New(t)
	is.Equal(pathAncestors("a"), []string{"a"})
	is.Equal(pathAncestors("a.git/b/c"), []string{"a.git/b/c", "a.git/b", "a"})
}

// LFS JSON endpoints answer media type errors in JSON: 406 for the Accept
// header and 415 for the Content-Type header. Responses carry no Accept
// header.
func TestLFSMediaTypes(t *testing.T) {
	is := is.New(t)
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
		is.Equal(w.Code, tc.code)
		is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)
		is.Equal(w.Header().Get("Accept"), "")
		var resp map[string]any
		is.NoErr(json.Unmarshal(w.Body.Bytes(), &resp))
	}

	w := f.send(http.MethodGet, "/secret.git/info/lfs/locks", map[string]string{"Authorization": auth}, "")
	is.Equal(w.Code, http.StatusNotAcceptable)
	is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)
}

// The batch API answers 409 to a hash algorithm other than SHA-256.
func TestLFSBatchHashAlgo(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := f.ownerAuth(t)
	hdr := map[string]string{"Authorization": auth, "Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	objects := `"objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]`

	w := f.send(http.MethodPost, "/secret.git/info/lfs/objects/batch", hdr, `{"operation":"download","hash_algo":"sha512",`+objects+`}`)
	is.Equal(w.Code, http.StatusConflict)
	is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)

	w = f.send(http.MethodPost, "/secret.git/info/lfs/objects/batch", hdr, `{"operation":"download","hash_algo":"sha256",`+objects+`}`)
	is.Equal(w.Code, http.StatusOK)
}

// Bad credentials get the same JSON 403 from LFS whether or not the caller
// could read the repository anonymously.
func TestLFSBadCredentialsAreJSON(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "public", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	badBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("owner:wrong-password"))
	hdr := map[string]string{"Authorization": badBasic, "Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	var want string
	for _, repo := range []string{"secret", "public"} {
		w := f.send(http.MethodPost, "/"+repo+".git/info/lfs/objects/batch", hdr, `{}`)
		is.Equal(w.Code, http.StatusForbidden)
		is.Equal(w.Header().Get("Content-Type"), lfs.MediaType)
		if want == "" {
			want = w.Body.String()
		}
		is.Equal(w.Body.String(), want)
	}
}

// withParams leaves the request URL alone, so request logs show the path
// the client asked for.
func TestWithParamsKeepsURL(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	var inner string
	router := mux.NewRouter()
	router.Handle("/{repo:.*}/info/refs", withParams(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inner = r.URL.Path
		is.Equal(mux.Vars(r)["repo"], "a/b")
	})))
	req := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/a/b.git/info/refs", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	is.Equal(inner, "/a/b.git/info/refs")
	is.Equal(req.URL.Path, "/a/b.git/info/refs")
}

// A panic is answered with 500 and logged with its stack on the request
// logger.
func TestRecoverPanics(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	h := recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequestWithContext(log.WithContext(context.Background(), logger.With("path", "/x")), http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	is.Equal(w.Code, http.StatusInternalServerError)
	out := logs.String()
	is.True(strings.Contains(out, "boom"))
	is.True(strings.Contains(out, "path=/x"))
	is.True(strings.Contains(out, "TestRecoverPanics"))

	// http.ErrAbortHandler still aborts the response.
	abort := recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		is.Equal(recover(), http.ErrAbortHandler)
	}()
	abort.ServeHTTP(httptest.NewRecorder(), req)
}

// Failed TLS handshakes are logged at debug level; other server errors
// stay errors.
func TestServerErrorLog(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.InfoLevel)
	l := stdlog.New(serverErrorLog{logger}, "", 0)

	l.Printf("http: TLS handshake error from 127.0.0.1:1234: EOF")
	is.Equal(logs.String(), "")

	l.Printf("http: Accept error: boom")
	is.True(strings.Contains(logs.String(), "ERRO"))
	is.True(strings.Contains(logs.String(), "http: Accept error: boom"))
}
