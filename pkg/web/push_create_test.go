package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/gorilla/mux"
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

// pushCreateTimeout is far longer than any test, so no removal timer fires
// on its own and nothing a test checks races one. A request arms or stops its repository's timer before it
// returns, so a test reads the timer's state once the request is done, and
// fires the real timer itself when it wants the removal to run.

// pendingState reports whether the repository name is on f's list of
// push-created repositories, and whether its removal timer is armed.
func (f *oracleFixture) pendingState(name string) (listed, armed bool) {
	f.pending.mu.Lock()
	defer f.pending.mu.Unlock()
	pc, ok := f.pending.repos[name]
	return ok, ok && pc.timer != nil
}

// pendingEntry returns the list entry of the repository name, the
// generation it is at, and its removal timer, as a timer armed now would
// see them.
func (f *oracleFixture) pendingEntry(t *testing.T, name string) (*pushCreate, int, *time.Timer) {
	t.Helper()
	f.pending.mu.Lock()
	defer f.pending.mu.Unlock()
	pc, ok := f.pending.repos[name]
	if !ok {
		t.Fatalf("%q is not listed", name)
	}
	return pc, pc.gen, pc.timer
}

// fireTimer makes the armed removal timer of the repository name fire now.
func (f *oracleFixture) fireTimer(t *testing.T, name string) {
	t.Helper()
	f.pending.mu.Lock()
	defer f.pending.mu.Unlock()
	pc, ok := f.pending.repos[name]
	if !ok || pc.timer == nil {
		t.Fatalf("no removal timer armed for %q", name)
	}
	pc.timer.Reset(0)
}

// waitUnlisted waits, up to a minute, until the repository name has left
// f's list: the removal its timer started has ended, whether it removed the
// repository or kept it.
func (f *oracleFixture) waitUnlisted(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if listed, _ := f.pendingState(name); !listed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q still listed a minute after its removal timer fired", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wantArmed fails the test unless the removal timer of the repository name
// is armed as want says.
func (f *oracleFixture) wantArmed(t *testing.T, name string, want bool) {
	t.Helper()
	if _, armed := f.pendingState(name); armed != want {
		t.Fatalf("removal timer of %q armed = %v, want %v", name, armed, want)
	}
}

// A repository created by an HTTP push advertisement is removed when the
// push that follows fails, or never arrives, and leaves no ref. One that has
// refs is kept, and so is one that a push left without refs but without
// failing, such as the probe git sends before a large pack.
func TestHTTPPushCreateCleanup(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	// advertise sends the ref advertisement that creates name, which arms
	// its removal timer.
	advertise := func(name string) {
		t.Helper()
		if got := f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
			t.Fatalf("f.do(http.MethodGet, \"/\"+name+\".git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
		}
		if !f.exists(name) {
			t.Fatal("not true: f.exists(name)")
		}
		f.wantArmed(t, name, true)
	}

	// The push fails: the repository is removed at once.
	advertise("failed")
	if got := f.push(t, "failed", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"failed\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	if f.exists("failed") {
		t.Fatal("not true: !f.exists(\"failed\")")
	}
	listed, _ := f.pendingState("failed")
	if listed {
		t.Fatal("not true: !listed")
	}

	// A push that updates no ref and does not fail, like git's probe
	// before a pack larger than http.postBuffer, keeps the repository for
	// the real push, and arms the timer again. With no push after it, it
	// goes when that timer fires.
	//
	// The probe stops the advertisement's timer, and a removal that timer
	// started anyway, having fired as the probe began, removes nothing:
	// the probe changed the generation it was armed with. Both are checked
	// on the advertisement's own timer and generation.
	advertise("probed")
	adPC, adGen, adTimer := f.pendingEntry(t, "probed")
	if got := f.push(t, "probed", auth, "0000"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"probed\", auth, \"0000\") = %v, want %v", got, http.StatusOK)
	}
	if adTimer.Stop() {
		t.Fatal("the advertisement's removal timer was still armed after the probe")
	}
	f.pending.remove("probed", adPC, adGen)
	if !f.exists("probed") {
		t.Fatal("not true: f.exists(\"probed\")")
	}
	listed, _ = f.pendingState("probed")
	if !listed {
		t.Fatal("not true: listed")
	}
	f.wantArmed(t, "probed", true)
	f.fireTimer(t, "probed")
	f.waitUnlisted(t, "probed")
	if f.exists("probed") {
		t.Fatal("not true: !f.exists(\"probed\")")
	}

	// The push never arrives: the repository goes when the timer the
	// advertisement armed fires.
	advertise("abandoned")
	f.fireTimer(t, "abandoned")
	f.waitUnlisted(t, "abandoned")
	if f.exists("abandoned") {
		t.Fatal("not true: !f.exists(\"abandoned\")")
	}

	// A repository that got refs is kept, even after a failed push. The
	// refs are written here outside any request. Once a request to it has
	// ended with refs, it leaves the list, so no timer can remove it.
	// The advertisement's removal, run after the push as a timer that
	// fired late would, removes nothing.
	advertise("kept")
	adPC, adGen, _ = f.pendingEntry(t, "kept")
	work := filepath.Join(t.TempDir(), "work")
	gitRun(t, "init", "-q", "-b", "master", work)
	gitRun(t, "-C", work, "commit", "-q", "--allow-empty", "-m", "first")
	dir := filepath.Join(f.env.cfg.DataPath, "repos", "kept.git")
	gitRun(t, "-C", dir, "fetch", "-q", work, "master:refs/heads/master")
	if got := f.push(t, "kept", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"kept\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	listed, _ = f.pendingState("kept")
	if listed {
		t.Fatal("not true: !listed")
	}
	f.pending.remove("kept", adPC, adGen)
	if !f.exists("kept") {
		t.Fatal("not true: f.exists(\"kept\")")
	}

	// The timer finds the refs too: a repository whose refs were written
	// with no request to it after the advertisement is kept when the
	// advertisement's timer fires.
	advertise("refs-only")
	dir = filepath.Join(f.env.cfg.DataPath, "repos", "refs-only.git")
	gitRun(t, "-C", dir, "fetch", "-q", work, "master:refs/heads/master")
	f.fireTimer(t, "refs-only")
	f.waitUnlisted(t, "refs-only")
	if !f.exists("refs-only") {
		t.Fatal("not true: f.exists(\"refs-only\")")
	}
}

// A repository created by an HTTP push and renamed before its push fails
// leaves another, empty repository created under its old name alone.
func TestHTTPPushCreateCleanupChecksID(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	if got := f.do(http.MethodGet, "/name.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/name.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	if err := f.be.RenameRepository(ownerCtx, "name", "moved"); err != nil {
		t.Fatal(err)
	}
	_, err := f.be.CreateRepository(ownerCtx, "name", f.owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.be.AddCollaborator(ownerCtx, "name", "stranger", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}

	// The failed push reaches the new repository under the listed name.
	if got := f.push(t, "name", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"name\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	if !f.exists("name") {
		t.Fatal("not true: f.exists(\"name\")")
	}
	if !f.exists("moved") {
		t.Fatal("not true: f.exists(\"moved\")")
	}
}

// A repository created by an HTTP push is kept when its push fails if
// anything was done with it meanwhile, such as adding a collaborator or
// changing its description: removing it would lose that work.
func TestHTTPPushCreateCleanupKeepsUsed(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)

	if got := f.do(http.MethodGet, "/collab.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/collab.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	if err := f.be.AddCollaborator(ownerCtx, "collab", "owner", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if got := f.push(t, "collab", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"collab\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	if !f.exists("collab") {
		t.Fatal("not true: f.exists(\"collab\")")
	}

	if got := f.do(http.MethodGet, "/described.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/described.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	if err := f.be.SetDescription(ownerCtx, "described", "about"); err != nil {
		t.Fatal(err)
	}
	if got := f.push(t, "described", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"described\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	if !f.exists("described") {
		t.Fatal("not true: f.exists(\"described\")")
	}
}

// A push over SSH to a repository an HTTP push created, running when that
// repository's HTTP push fails, keeps the repository: the SSH push may write
// refs at any moment.
func TestHTTPPushCreateCleanupWaitsForOtherPushes(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token

	if got := f.do(http.MethodGet, "/shared.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/shared.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	repo, err := f.be.Repository(f.ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.be.BeginPush(f.ctx, repo) // as the SSH push does
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	if got := f.push(t, "shared", auth, "garbage"); got != http.StatusOK {
		t.Fatalf("f.push(t, \"shared\", auth, \"garbage\") = %v, want %v", got, http.StatusOK)
	}
	if !f.exists("shared") {
		t.Fatal("not true: f.exists(\"shared\")")
	}
}

// A request that waits for a removal of a push-created repository resolves
// the repository and the caller's access again once the removal is over: it
// finds the repository gone if it was removed, rather than running against
// the deleted one, and proceeds if it was kept.
func TestHTTPPushCreateWaitedRequestResolvesAgain(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	token, err := f.be.CreateAccessToken(f.ctx, f.owner, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	s := f.env.server(t)
	pending := s.pending
	waits := make(chan string, 1)
	pending.waiting = func(name string) { waits <- name }
	var reached proto.Repository
	next := func(_ *server, w http.ResponseWriter, _ *http.Request, gr *gitRequest) {
		reached = gr.repo
		w.WriteHeader(http.StatusNoContent)
	}
	router := mux.NewRouter()
	router.Handle("/{repo:.*}/info/refs", gitRoute{
		method:  []string{http.MethodGet},
		handler: next,
		path:    "/info/refs",
		service: true,
		server:  s,
	})
	handler := router

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
			if got := got; got != name {
				t.Fatalf("got = %v, want %v", got, name)
			}
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
	if err != nil {
		t.Fatal(err)
	}
	code := fetch("removed", func() {
		if err := f.be.DeleteRepository(ownerCtx, "removed"); err != nil {
			t.Fatal(err)
		}
	})
	if reached != nil {
		t.Fatal("not true: reached == nil")
	}
	if got := code; got != http.StatusNotFound {
		t.Fatalf("code = %v, want %v", got, http.StatusNotFound)
	}

	// Kept: the request proceeds.
	kept, err := f.be.CreateRepository(ownerCtx, "kept", f.owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch("kept", func() {}); got != http.StatusNoContent {
		t.Fatalf("fetch(\"kept\", func() {}) = %v, want %v", got, http.StatusNoContent)
	}
	if reached == nil {
		t.Fatal("not true: reached != nil")
	}
	if got, want := reached.ID(), kept.ID(); got != want {
		t.Fatalf("reached.ID() = %v, want %v", got, want)
	}
}

// Once the list is closed, as the HTTP server does when it stops, no
// repository created by a push is removed: the timers already armed are
// stopped and none is armed, so no removal runs against a backend that is
// being closed.
func TestHTTPPushCreateCloseStopsRemovals(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	advertise := func(name string) {
		t.Helper()
		if got := f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
			t.Fatalf("f.do(http.MethodGet, \"/\"+name+\".git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
		}
		if !f.exists(name) {
			t.Fatal("not true: f.exists(name)")
		}
	}

	advertise("armed")
	f.wantArmed(t, "armed", true)
	pc, gen, _ := f.pendingEntry(t, "armed")
	if err := f.pending.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantArmed(t, "armed", false)
	advertise("late")
	f.wantArmed(t, "late", false)
	// A timer that fired as close stopped it removes nothing: run what
	// it would have run.
	f.pending.remove("armed", pc, gen)
	if !f.exists("armed") {
		t.Fatal("not true: f.exists(\"armed\")")
	}
	if !f.exists("late") {
		t.Fatal("not true: f.exists(\"late\")")
	}
}

// A push to a name that is not a valid repository name is refused with
// 400 and the reason, rather than failing as a server error.
func TestHTTPPushCreateInvalidName(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token

	w := f.do(http.MethodGet, "/bad%20name.git/info/refs?service=git-receive-pack", auth)
	if got := w.Code; got != http.StatusBadRequest {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "repo can only contain") {
		t.Fatal("not true: strings.Contains(w.Body.String(), \"repo can only contain\")")
	}
	if got := f.push(t, "bad%20name", auth, "0000"); got != http.StatusBadRequest {
		t.Fatalf("f.push(t, \"bad%%20name\", auth, \"0000\") = %v, want %v", got, http.StatusBadRequest)
	}
	if f.exists("bad name") {
		t.Fatal("not true: !f.exists(\"bad name\")")
	}
}

// A push to a missing name that another request creates first is
// authorized for the repository now there, rather than failing as a
// server error.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestHTTPPushCreateRace(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	stranger, err := f.be.User(f.ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	advertise := func(name string) int {
		t.Helper()
		return f.do(http.MethodGet, "/"+name+".git/info/refs?service=git-receive-pack", auth).Code
	}
	createFirst := func(name string, owner proto.User, private bool) {
		f.server.beforePushCreate = func() {
			f.server.beforePushCreate = nil
			_, err := f.be.CreateRepository(f.ctx, name, owner, proto.RepositoryOptions{Private: private})
			if err != nil {
				t.Error(err)
			}
		}
	}

	t.Run("same user", func(t *testing.T) {
		createFirst("mine", stranger, false)
		if got := advertise("mine"); got != http.StatusOK {
			t.Fatalf("advertise(\"mine\") = %v, want %v", got, http.StatusOK)
		}
		// This request did not create it, so it does not list it for
		// removal, and no timer can remove it.
		listed, _ := f.pendingState("mine")
		if listed {
			t.Fatal("not true: !listed")
		}
		if !f.exists("mine") {
			t.Fatal("not true: f.exists(\"mine\")")
		}
	})

	t.Run("public repository of another user", func(t *testing.T) {
		createFirst("theirs", f.owner, false)
		if got := advertise("theirs"); got != http.StatusForbidden {
			t.Fatalf("advertise(\"theirs\") = %v, want %v", got, http.StatusForbidden)
		}
	})

	t.Run("private repository of another user", func(t *testing.T) {
		createFirst("hidden", f.owner, true)
		if got := advertise("hidden"); got != http.StatusNotFound {
			t.Fatalf("advertise(\"hidden\") = %v, want %v", got, http.StatusNotFound)
		}
	})

	// A creation still in progress, which has claimed the directory but
	// not yet registered the repository, is a taken name like any other
	// the caller cannot read.
	t.Run("creation in progress", func(t *testing.T) {
		f.server.beforePushCreate = func() {
			f.server.beforePushCreate = nil
			dir := filepath.Join(f.env.cfg.DataPath, "repos", "claimed.git")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
		}
		if got := advertise("claimed"); got != http.StatusNotFound {
			t.Fatalf("advertise(\"claimed\") = %v, want %v", got, http.StatusNotFound)
		}
		if f.exists("claimed") {
			t.Fatal("not true: !f.exists(\"claimed\")")
		}
	})
}

// A failed push can have written refs before it failed: receive-pack
// updates them before the hooks run. The repository is then handed to the
// backend's removal, which keeps it and points HEAD at the pushed branch,
// not at a branch that does not exist.
func TestHTTPPushCreateFailedPushWithRefsGetsHead(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	auth := "Token " + f.token
	if got := f.do(http.MethodGet, "/pushed.git/info/refs?service=git-receive-pack", auth).Code; got != http.StatusOK {
		t.Fatalf("f.do(http.MethodGet, \"/pushed.git/info/refs?service=git-receive-pack\", auth).Code = %v, want %v", got, http.StatusOK)
	}
	dir := filepath.Join(f.env.cfg.DataPath, "repos", "pushed.git")
	gitRun(t, "-C", dir, "symbolic-ref", "HEAD", "refs/heads/missing")
	tree := gitOutput(t, "-C", dir, "mktree")
	commit := gitOutput(t, "-C", dir, "commit-tree", tree, "-m", "x")
	gitRun(t, "-C", dir, "update-ref", "refs/heads/feature", commit)

	// A receive-pack request that fails after its refs landed.
	done, _ := f.pending.begin("pushed")
	if done == nil {
		t.Fatal("not true: done != nil")
	}
	done(true)

	if !f.exists("pushed") {
		t.Fatal("not true: f.exists(\"pushed\")")
	}
	listed, _ := f.pendingState("pushed")
	if listed {
		t.Fatal("not true: !listed")
	}
	if got := gitOutput(t, "-C", dir, "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("gitOutput(t, \"-C\", dir, \"symbolic-ref\", \"HEAD\") = %v, want %v", got, "refs/heads/feature")
	}
}

// gitOutput runs git and returns its trimmed output, failing the test on
// error.
func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// panicRepo is a repository whose name panics, so a removal of it panics.
type panicRepo struct{ proto.Repository }

func (panicRepo) Name() string { panic("panicRepo.Name") }

// A panic in a removal its timer runs is recovered and logged; it does not
// end the process, and the repository still leaves the list.
func TestHTTPPushCreateTimerRecoversPanic(t *testing.T) {
	t.Parallel()
	f := newOracleFixture(t)
	if got := f.do(http.MethodGet, "/boom.git/info/refs?service=git-receive-pack", "Token "+f.token).Code; got != http.StatusOK {
		t.Fatalf("advertisement = %v, want %v", got, http.StatusOK)
	}
	var logs syncLog
	f.pending.mu.Lock()
	f.pending.repos["boom"].repo = panicRepo{f.pending.repos["boom"].repo}
	f.pending.logger = log.New(&logs)
	f.pending.mu.Unlock()

	f.fireTimer(t, "boom")
	f.waitUnlisted(t, "boom")
	for deadline := time.Now().Add(time.Minute); !strings.Contains(logs.String(), "recovered panic"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no recovered panic logged a minute after the timer fired: %q", logs.String())
		}
	}
}

// syncLog is a buffer a logger writes to from another goroutine.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
