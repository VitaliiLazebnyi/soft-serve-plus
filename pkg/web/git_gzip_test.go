package web

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

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
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	_, err := f.be.CreateRepository(ownerCtx, "public", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	w := f.gzipRPC(t, "public", "git-upload-pack", auth, []byte("not gzip"))
	is.Equal(w.Code, http.StatusBadRequest)
	is.True(!strings.Contains(w.Header().Get("Content-Type"), "result"))

	is.Equal(f.do(http.MethodGet, "/created.git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	is.True(f.exists("created"))
	w = f.gzipRPC(t, "created", "git-receive-pack", auth, []byte("not gzip"))
	is.Equal(w.Code, http.StatusBadRequest)
	is.True(!f.exists("created"))

	// A well-formed gzip body is still served.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err = zw.Write([]byte("0000"))
	is.NoErr(err)
	is.NoErr(zw.Close())
	w = f.gzipRPC(t, "public", "git-upload-pack", auth, buf.Bytes())
	is.Equal(w.Code, http.StatusOK)
	is.Equal(w.Header().Get("Content-Type"), "application/x-git-upload-pack-result")
}
