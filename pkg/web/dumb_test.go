package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestDumbHTTPIsRefused checks every request of the dumb HTTP protocol gets
// one uncacheable 403, whatever the repository and the caller. A missing
// loose object used to be answered 404 with a one-year Cache-Control.
func TestDumbHTTPIsRefused(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	pubDir := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", "pub.git")
	looseObj := "objects/12/" + strings.Repeat("3", 38)
	is.NoErr(os.MkdirAll(filepath.Join(pubDir, "objects", "12"), 0o755))
	is.NoErr(os.WriteFile(filepath.Join(pubDir, filepath.FromSlash(looseObj)), []byte("data"), 0o644))

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
					is.Equal(w.Code, http.StatusForbidden) // dumb request
					is.Equal(w.Header().Get("Cache-Control"), "no-cache, max-age=0, must-revalidate")
					is.Equal(w.Header().Get("Expires"), "Fri, 01 Jan 1980 00:00:00 GMT")
					is.Equal(w.Header().Get("WWW-Authenticate"), "") // no credentials help
					if method == http.MethodGet {
						is.Equal(w.Body.String(), errDumbHTTP.Error()+"\n")
					}
				}
			}
		}
	}

	// Other methods are refused as before, and nothing was created.
	w := f.do(http.MethodPost, "/pub.git/HEAD", "")
	is.Equal(w.Code, http.StatusMethodNotAllowed)
	is.Equal(w.Header().Get("Allow"), "GET, HEAD")
	_, err = f.be.Repository(f.ctx, "ghost")
	is.True(err != nil)

	// A smart client still gets the advertisement.
	is.Equal(f.do(http.MethodGet, "/pub.git/info/refs?service=git-upload-pack", "").Code, http.StatusOK)
}

// TestDumbCloneFails checks a client forced to the dumb protocol is told why
// it cannot clone.
func TestDumbCloneFails(t *testing.T) {
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
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "group/HEAD", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	for _, tc := range []struct{ path, repo string }{
		{"/group/HEAD?go-get=1", "group/HEAD"},
		{"/group/HEAD/objects/info/packs?go-get=1", "group/HEAD"}, // a package inside it
		{"/pub/info/refs?go-get=1", "pub"},                        // a package "info/refs"
		{"/pub.git/info/refs?go-get=1", "pub"},
	} {
		w := f.do(http.MethodGet, tc.path, "")
		is.Equal(w.Code, http.StatusOK) // go get page
		is.True(strings.Contains(w.Body.String(), `name="go-import"`))
		is.True(strings.Contains(w.Body.String(), "/"+tc.repo+" git "))
	}

	// Without go-get=1, and with another method, it is still refused.
	is.Equal(f.do(http.MethodGet, "/group/HEAD", "").Code, http.StatusForbidden)
	is.Equal(f.do(http.MethodGet, "/pub/info/refs", "").Code, http.StatusForbidden)
	// A smart ref advertisement is still one, go-get=1 or not.
	is.Equal(f.do(http.MethodGet, "/pub/info/refs?service=git-upload-pack&go-get=1", "").Code, http.StatusOK)
	w := f.do(http.MethodPost, "/group/HEAD?go-get=1", "")
	is.Equal(w.Code, http.StatusMethodNotAllowed)
	is.Equal(w.Header().Get("Allow"), "GET, HEAD")
}
