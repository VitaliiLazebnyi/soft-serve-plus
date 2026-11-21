package web

import (
	"net/http"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// With allow-keyless off, an anonymous caller is asked for credentials on
// the git routes and on go get, even for a public repository, and a caller
// with a token is served.
func TestKeylessDisabledRequiresCaller(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(f.be.SetAllowKeyless(f.ctx, false))

	auth := "Token " + f.token
	for _, path := range []string{"/pub.git/info/refs?service=git-upload-pack", "/pub?go-get=1"} {
		w := f.do(http.MethodGet, path, "")
		is.Equal(w.Code, http.StatusUnauthorized) // anonymous
		is.True(w.Header().Get("WWW-Authenticate") != "")
		is.Equal(f.do(http.MethodGet, path, auth).Code, http.StatusOK) // with a token
	}
}
