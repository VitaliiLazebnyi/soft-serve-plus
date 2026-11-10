package web

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// httpGitCounts returns the HTTP fetch and push counters for repo.
func httpGitCounts(repo string) (fetches, pushes float64) {
	return testutil.ToFloat64(gitHttpUploadCounter.WithLabelValues(repo, "info/refs")),
		testutil.ToFloat64(gitHttpReceiveCounter.WithLabelValues(repo))
}

// Fetches and pushes over HTTP are counted on their ref advertisement only:
// neither the RPCs that follow it nor refused requests are counted.
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

	fetches, pushes := httpGitCounts(name)
	secretFetches, _ := httpGitCounts("secret")

	// A fetch advertisement and a push advertisement count once each.
	is.Equal(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", "").Code, http.StatusOK)
	is.Equal(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	// The RPCs that follow an advertisement belong to the same fetch or
	// push, like the flush-only probe git sends before a large pack.
	is.Equal(rpc(name, "git-upload-pack", ""), http.StatusOK)
	is.Equal(rpc(name, "git-receive-pack", auth), http.StatusOK)
	// Refused: an anonymous push, and a fetch of a private repository.
	is.True(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", "").Code != http.StatusOK)
	is.True(rpc(name, "git-receive-pack", "") != http.StatusOK)
	is.True(f.do(http.MethodGet, "/secret.git/info/refs?service=git-upload-pack", "").Code != http.StatusOK)
	is.True(rpc("secret", "git-upload-pack", "") != http.StatusOK)

	gotFetches, gotPushes := httpGitCounts(name)
	is.Equal(gotFetches-fetches, 1.0)
	is.Equal(gotPushes-pushes, 1.0)
	gotSecret, _ := httpGitCounts("secret")
	is.Equal(gotSecret-secretFetches, 0.0)
}

// A clone or fetch by a real git client is counted once, whichever protocol
// version it speaks, although it sends more than one RPC.
func TestGitHTTPFetchCountedOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	is := is.New(t)
	f := newOracleFixture(t)
	const name = "http-fetch-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	is.NoErr(err)

	// Give the repository a commit, so a clone fetches a pack. Its hooks run
	// the server binary, which is the test binary here; push with no hooks.
	work := t.TempDir()
	gitRun(t, "init", "-q", work)
	gitRun(t, "-C", work, "commit", "-q", "--allow-empty", "-m", "first")
	bare := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", name+".git")
	gitRun(t, "-C", work, "-c", "core.hooksPath="+t.TempDir(), "push", "-q", bare, "HEAD:refs/heads/main")

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)
	url := srv.URL + "/" + name + ".git"

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
		is.Equal(counted("-c", "protocol.version="+version, "clone", "-q", url, dst), 1.0)
		// A fetch with nothing new sends no RPC in v0, and ls-refs in v2.
		is.Equal(counted("-C", dst, "-c", "protocol.version="+version, "fetch", "-q", "origin"), 1.0)
	}
}

// A push by a real git client is counted once, even when its pack is larger
// than http.postBuffer and git sends a probe RPC before the real one, and
// even when it has nothing to send and sends no RPC.
func TestGitHTTPPushCountedOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	is := is.New(t)
	f := newOracleFixture(t)
	const name = "http-push-counted"
	_, err := f.be.CreateRepository(f.ctx, name, f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	// The repository's hooks run the server binary, which is the test binary
	// here, and would refuse the push.
	hooks := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", name+".git", "hooks")
	is.NoErr(os.RemoveAll(hooks))
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	is.NoErr(err)

	srv := httptest.NewServer(f.handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/" + name + ".git")
	is.NoErr(err)
	u.User = url.UserPassword("owner", token)

	// A commit with a file that does not compress, larger than the post
	// buffer below.
	work := t.TempDir()
	gitRun(t, "init", "-q", work)
	blob := make([]byte, 256<<10)
	_, err = rand.Read(blob)
	is.NoErr(err)
	is.NoErr(os.WriteFile(filepath.Join(work, "blob"), blob, 0o600))
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
	is.Equal(counted(push...), 1.0)
	// Everything is up to date: git reads the advertisement and stops.
	is.Equal(counted(push...), 1.0)
}
