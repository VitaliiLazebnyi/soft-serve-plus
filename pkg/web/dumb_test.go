package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// writeRepoFile writes a file inside a repository's git directory.
func writeRepoFile(t *testing.T, f *oracleFixture, repo, name, content string) {
	t.Helper()
	is := is.New(t)
	p := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", repo+".git", filepath.FromSlash(name))
	is.NoErr(os.MkdirAll(filepath.Dir(p), 0o755))
	is.NoErr(os.WriteFile(p, []byte(content), 0o644))
}

// TestDumbHTTPServesOnlyRegularFiles checks that a dumb HTTP path that does
// not name a readable regular file is not found, rather than a directory
// listing or a panic.
func TestDumbHTTPServesOnlyRegularFiles(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	// objects/ab is a file, so stat of a loose object below it fails with
	// ENOTDIR rather than not-exist.
	writeRepoFile(t, f, "pub", "objects/ab", "x")
	// A directory where a loose object would be.
	is.NoErr(os.MkdirAll(filepath.Join(config.FromContext(f.ctx).DataPath, "repos", "pub.git", "objects", "cd", strings.Repeat("0", 38)), 0o755))

	for _, path := range []string{
		"/pub.git/objects/ab/" + strings.Repeat("0", 38),
		"/pub.git/objects/cd/" + strings.Repeat("0", 38),
		"/pub.git/objects/info/",
		"/pub.git/objects/info/" + strings.Repeat("a", 300),
		"/pub.git/objects/info/a%00b",
		"/pub.git/objects/info/commit-graph",
	} {
		w := f.do(http.MethodGet, path, "")
		is.Equal(w.Code, http.StatusNotFound) // path
	}
}

// TestDumbHTTPInfoPacksIsNotCached checks that objects/info/packs reaches
// its own handler, which lists the packs on disk rather than a stale
// objects/info/packs file, and is served uncached, as git http-backend does.
func TestDumbHTTPInfoPacksIsNotCached(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	pack := "pack-" + strings.Repeat("a", 40)
	writeRepoFile(t, f, "pub", "objects/pack/"+pack+".pack", "x")
	writeRepoFile(t, f, "pub", "objects/pack/"+pack+".idx", "x")
	writeRepoFile(t, f, "pub", "objects/info/packs", "P pack-x.pack\n")

	w := f.do(http.MethodGet, "/pub.git/objects/info/packs", "")
	is.Equal(w.Code, http.StatusOK)
	is.Equal(w.Body.String(), "P "+pack+".pack\n\n")
	is.Equal(w.Header().Get("Content-Type"), "text/plain; charset=utf-8")
	is.True(strings.HasPrefix(w.Header().Get("Cache-Control"), "no-cache"))
}

// TestDumbHTTPObjectCacheHeaders checks the headers of immutable objects:
// valid HTTP dates, and no shared caching of private or authenticated
// responses.
func TestDumbHTTPObjectCacheHeaders(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	ownerToken, err := f.be.CreateAccessToken(f.ctx, f.owner, "o", time.Now().Add(time.Hour))
	is.NoErr(err)

	sha1Obj := "objects/12/" + strings.Repeat("3", 38)
	sha256Obj := "objects/12/" + strings.Repeat("4", 62)
	sha256Pack := "objects/pack/pack-" + strings.Repeat("5", 64) + ".pack"
	for _, repo := range []string{"pub", "secret"} {
		for _, name := range []string{sha1Obj, sha256Obj, sha256Pack} {
			writeRepoFile(t, f, repo, name, "data")
		}
	}

	for _, tc := range []struct {
		repo, auth, scope string
	}{
		{"pub", "", "public"},
		{"pub", "Token " + ownerToken, "private"},
		{"secret", "Token " + ownerToken, "private"},
	} {
		for _, name := range []string{sha1Obj, sha256Obj, sha256Pack} {
			w := f.do(http.MethodGet, "/"+tc.repo+".git/"+name, tc.auth)
			is.Equal(w.Code, http.StatusOK) // repo and object
			is.Equal(w.Body.String(), "data")
			is.Equal(w.Header().Get("Cache-Control"), tc.scope+", max-age=31536000")
			_, err := http.ParseTime(w.Header().Get("Expires"))
			is.NoErr(err)                        // Expires is an HTTP-date
			is.Equal(w.Header().Get("Date"), "") // left to net/http
			_, err = http.ParseTime(w.Header().Get("Last-Modified"))
			is.NoErr(err)
		}
	}
}
