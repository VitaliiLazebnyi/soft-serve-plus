package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Each fetch and push RPC over HTTP is counted once, under its own service;
// ref advertisements and refused requests are not counted.
func TestGitHTTPCounters(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	const name = "http-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	is.NoErr(err)
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

	// Advertisements, for a fetch and for a push, are not RPCs.
	is.Equal(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", "").Code, http.StatusOK)
	is.Equal(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)

	is.Equal(rpc(name, "git-upload-pack", ""), http.StatusOK)
	is.Equal(rpc(name, "git-receive-pack", auth), http.StatusOK)
	// Refused: an anonymous push, and a fetch of a private repository.
	is.True(rpc(name, "git-receive-pack", "") != http.StatusOK)
	is.True(rpc("secret", "git-upload-pack", "") != http.StatusOK)

	is.Equal(testutil.ToFloat64(gitHttpUploadCounter.WithLabelValues(name, "info/refs")), 0.0)
	is.Equal(testutil.ToFloat64(gitHttpUploadCounter.WithLabelValues(name, "git-upload-pack")), 1.0)
	is.Equal(testutil.ToFloat64(gitHttpReceiveCounter.WithLabelValues(name)), 1.0)
	is.Equal(testutil.ToFloat64(gitHttpUploadCounter.WithLabelValues("secret", "git-upload-pack")), 0.0)
}
