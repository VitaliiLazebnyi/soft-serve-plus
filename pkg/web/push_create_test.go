package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/gorilla/mux"
	"github.com/matryer/is"
)

// push sends a receive-pack RPC with body and returns the status code.
func (f *oracleFixture) push(t *testing.T, name, auth, body string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/"+name+".git/git-receive-pack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w.Code
}

// exists reports whether the backend has the repository name.
func (f *oracleFixture) exists(name string) bool {
	_, err := f.be.Repository(f.ctx, name)
	return err == nil
}

// gitRun runs git and fails the test on error.
func gitRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// eventually waits until the repository name is gone, or a deadline.
func (f *oracleFixture) eventuallyGone(name string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for f.exists(name) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return !f.exists(name)
}

// A repository created by an HTTP push advertisement is removed when the
// push that follows fails, or never arrives, and leaves no ref. One that has
// refs is kept, and so is one that a push left without refs but without
// failing, such as the probe git sends before a large pack.
func TestHTTPPushCreateCleanup(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token
	advertise := func(name string) {
		t.Helper()
		is.Equal(f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
		is.True(f.exists(name))
	}
	old := pushCreateTimeout
	pushCreateTimeout = 200 * time.Millisecond
	t.Cleanup(func() { pushCreateTimeout = old })

	// The push fails: the repository is removed at once.
	advertise("failed")
	is.Equal(f.push(t, "failed", auth, "garbage"), http.StatusOK)
	is.True(!f.exists("failed"))

	// A push that updates no ref and does not fail, like git's probe
	// before a pack larger than http.postBuffer, keeps the repository for
	// the real push. With no push after it, it goes after the timeout.
	advertise("probed")
	is.Equal(f.push(t, "probed", auth, "0000"), http.StatusOK)
	is.True(f.exists("probed"))
	is.True(f.eventuallyGone("probed"))

	// The push never arrives.
	advertise("abandoned")
	is.True(f.eventuallyGone("abandoned"))

	// A repository that got refs is kept, even after a failed push and
	// the timeout. The refs are written here outside any request, which
	// the timeout armed by the advertisement does not wait for, and the
	// git processes that write them can take longer than a short timeout
	// to start (on Windows they did), so the timeout is long until the
	// refs are in.
	pushCreateTimeout = time.Hour
	advertise("kept")
	work := filepath.Join(t.TempDir(), "work")
	gitRun(t, "init", "-q", "-b", "master", work)
	gitRun(t, "-C", work, "commit", "-q", "--allow-empty", "-m", "first")
	dir := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", "kept.git")
	gitRun(t, "-C", dir, "fetch", "-q", work, "master:refs/heads/master")
	pushCreateTimeout = 200 * time.Millisecond
	is.Equal(f.push(t, "kept", auth, "garbage"), http.StatusOK)
	time.Sleep(3 * pushCreateTimeout)
	is.True(f.exists("kept"))
}

// A repository created by an HTTP push and renamed before its push fails
// leaves another, empty repository created under its old name alone.
func TestHTTPPushCreateCleanupChecksID(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	is.Equal(f.do(http.MethodGet, "/name.git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	is.NoErr(f.be.RenameRepository(ownerCtx, "name", "moved"))
	_, err := f.be.CreateRepository(ownerCtx, "name", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(f.be.AddCollaborator(ownerCtx, "name", "stranger", access.ReadWriteAccess))

	// The failed push reaches the new repository under the listed name.
	is.Equal(f.push(t, "name", auth, "garbage"), http.StatusOK)
	is.True(f.exists("name"))
	is.True(f.exists("moved"))
}

// A repository created by an HTTP push is kept when its push fails if
// anything was done with it meanwhile, such as adding a collaborator or
// changing its description: removing it would lose that work.
func TestHTTPPushCreateCleanupKeepsUsed(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	is.Equal(f.do(http.MethodGet, "/collab.git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	is.NoErr(f.be.AddCollaborator(ownerCtx, "collab", "owner", access.ReadOnlyAccess))
	is.Equal(f.push(t, "collab", auth, "garbage"), http.StatusOK)
	is.True(f.exists("collab"))

	is.Equal(f.do(http.MethodGet, "/described.git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	is.NoErr(f.be.SetDescription(ownerCtx, "described", "about"))
	is.Equal(f.push(t, "described", auth, "garbage"), http.StatusOK)
	is.True(f.exists("described"))
}

// A push over SSH to a repository an HTTP push created, running when that
// repository's HTTP push fails, keeps the repository: the SSH push may write
// refs at any moment.
func TestHTTPPushCreateCleanupWaitsForOtherPushes(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token

	is.Equal(f.do(http.MethodGet, "/shared.git/info/refs?service=git-receive-pack", auth).Code, http.StatusOK)
	repo, err := f.be.Repository(f.ctx, "shared")
	is.NoErr(err)
	done, err := f.be.BeginPush(f.ctx, repo) // as the SSH push does
	is.NoErr(err)
	defer done()

	is.Equal(f.push(t, "shared", auth, "garbage"), http.StatusOK)
	is.True(f.exists("shared"))
}

// A request that waits for a removal of a push-created repository resolves
// the repository and the caller's access again once the removal is over: it
// finds the repository gone if it was removed, rather than running against
// the deleted one, and proceeds if it was kept.
func TestHTTPPushCreateWaitedRequestResolvesAgain(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	is.NoErr(err)

	pending := newPushCreates()
	waits := make(chan string, 1)
	pending.waiting = func(name string) { waits <- name }
	var reached proto.Repository
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = proto.RepositoryFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	router := mux.NewRouter()
	router.Handle("/{repo:.*}/info/refs", withParams(GitRoute{
		method:  []string{http.MethodGet},
		handler: next,
		path:    "/info/refs",
		pending: pending,
		limits:  &requestLimits{},
	}))
	handler := NewContextHandler(f.ctx)(router)

	// fetch sends the owner's ref advertisement request for name while a
	// removal of name is running, and ends that removal with end once
	// the request waits for it.
	fetch := func(name string, end func()) int {
		t.Helper()
		ch := make(chan struct{})
		pending.mu.Lock()
		pending.repos[name] = &pushCreate{removing: ch}
		pending.mu.Unlock()

		w := httptest.NewRecorder()
		served := make(chan struct{})
		go func() {
			defer close(served)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+name+".git/info/refs?service=git-upload-pack", nil)
			req.Header.Set("Authorization", "Token "+token)
			handler.ServeHTTP(w, req)
		}()
		select {
		case got := <-waits:
			is.Equal(got, name)
		case <-served:
			t.Fatalf("request for %q ended without waiting for the removal", name)
		}
		end()
		pending.mu.Lock()
		delete(pending.repos, name)
		close(ch)
		pending.mu.Unlock()
		<-served
		return w.Code
	}

	// Removed: the request finds the repository gone.
	_, err = f.be.CreateRepository(ownerCtx, "removed", f.owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	code := fetch("removed", func() { is.NoErr(f.be.DeleteRepository(ownerCtx, "removed")) })
	is.True(reached == nil)
	is.Equal(code, http.StatusNotFound)

	// Kept: the request proceeds.
	kept, err := f.be.CreateRepository(ownerCtx, "kept", f.owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	is.Equal(fetch("kept", func() {}), http.StatusNoContent)
	is.True(reached != nil)
	is.Equal(reached.ID(), kept.ID())
}

// A repository whose refs cannot be listed counts as having refs, so the
// cleanup never removes one it could not check.
func TestHasRefsFailsSafe(t *testing.T) {
	is := is.New(t)
	is.True(hasRefs(filepath.Join(t.TempDir(), "missing.git")))

	dir := filepath.Join(t.TempDir(), "empty.git")
	gitRun(t, "init", "-q", "--bare", dir)
	is.True(!hasRefs(dir))
}

// A push to a name that is not a valid repository name is refused with
// 400 and the reason, rather than failing as a server error.
func TestHTTPPushCreateInvalidName(t *testing.T) {
	is := is.New(t)
	f := newOracleFixture(t)
	auth := "Token " + f.token

	w := f.do(http.MethodGet, "/bad%20name.git/info/refs?service=git-receive-pack", auth)
	is.Equal(w.Code, http.StatusBadRequest)
	is.True(strings.Contains(w.Body.String(), "repo can only contain"))
	is.Equal(f.push(t, "bad%20name", auth, "0000"), http.StatusBadRequest)
	is.True(!f.exists("bad name"))
}

// A push to a missing name that another request creates first is
// authorized for the repository now there, rather than failing as a
// server error.
func TestHTTPPushCreateRace(t *testing.T) {
	f := newOracleFixture(t)
	auth := "Token " + f.token
	stranger, err := f.be.User(f.ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	old := pushCreateTimeout
	pushCreateTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		pushCreateTimeout = old
		testHookBeforePushCreate = nil
	})
	advertise := func(name string) int {
		t.Helper()
		return f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code
	}
	createFirst := func(name string, owner proto.User, private bool) {
		testHookBeforePushCreate = func() {
			testHookBeforePushCreate = nil
			_, err := f.be.CreateRepository(f.ctx, name, owner, proto.RepositoryOptions{Private: private})
			if err != nil {
				t.Error(err)
			}
		}
	}

	t.Run("same user", func(t *testing.T) {
		is := is.New(t)
		createFirst("mine", stranger, false)
		is.Equal(advertise("mine"), http.StatusOK)
		// This request did not create it, so it does not remove it.
		time.Sleep(3 * pushCreateTimeout)
		is.True(f.exists("mine"))
	})

	t.Run("public repository of another user", func(t *testing.T) {
		is := is.New(t)
		createFirst("theirs", f.owner, false)
		is.Equal(advertise("theirs"), http.StatusForbidden)
	})

	t.Run("private repository of another user", func(t *testing.T) {
		is := is.New(t)
		createFirst("hidden", f.owner, true)
		is.Equal(advertise("hidden"), http.StatusNotFound)
	})

	// A creation still in progress, which has claimed the directory but
	// not yet registered the repository, is a taken name like any other
	// the caller cannot read.
	t.Run("creation in progress", func(t *testing.T) {
		is := is.New(t)
		testHookBeforePushCreate = func() {
			testHookBeforePushCreate = nil
			dir := filepath.Join(config.FromContext(f.ctx).DataPath, "repos", "claimed.git")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
		}
		is.Equal(advertise("claimed"), http.StatusNotFound)
		is.True(!f.exists("claimed"))
	})
}
