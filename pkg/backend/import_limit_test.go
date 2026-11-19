package backend

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// stallingRemote returns an ssh remote on loopback whose server accepts the
// connection and never says anything, so a clone from it hangs until it is
// stopped. accepted receives once per connection.
func stallingRemote(t *testing.T) (remote, hostPort string, accepted <-chan struct{}) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 16)
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			ch <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	hostPort = l.Addr().String()
	return "ssh://git@" + hostPort + "/x.git", hostPort, ch
}

// waitAccepted waits for the stalling remote to be dialed.
func waitAccepted(t *testing.T, accepted <-chan struct{}) {
	t.Helper()
	select {
	case <-accepted:
	case <-time.After(time.Minute):
		t.Fatal("the import never reached the remote")
	}
}

// importAsync runs ImportRepository in the background and returns its error.
func importAsync(ctx context.Context, be *Backend, name string, user proto.User, remote string) <-chan error {
	errc := make(chan error, 1)
	go func() {
		_, err := be.ImportRepository(ctx, name, user, remote, proto.RepositoryOptions{})
		errc <- err
	}()
	return errc
}

// waitImport waits for an import started with importAsync to return.
func waitImport(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(time.Minute):
		t.Fatal("the import did not stop")
		return nil
	}
}

// assertNothingLeft checks that a stopped import left no repository behind.
func assertNothingLeft(t *testing.T, be *Backend, name string) {
	t.Helper()
	is := is.New(t)
	_, err := be.Repository(context.Background(), name)
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	_, err = os.Stat(be.repoPath(name))
	is.True(errors.Is(err, os.ErrNotExist))
}

// An import stops, and cleans up, when the caller goes away: a dropped SSH
// session must not leave a clone running on the server.
func TestImportRepositoryStopsWithCaller(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	remote, _, accepted := stallingRemote(t)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	errc := importAsync(ctx, be, "stalled", admin, remote)
	waitAccepted(t, accepted)
	cancel()
	is.True(errors.Is(waitImport(t, errc), proto.ErrImportFailed))
	assertNothingLeft(t, be, "stalled")
}

// An import that runs past importTimeout is stopped and cleaned up.
func TestImportRepositoryTimeout(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	remote, _, _ := stallingRemote(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	prev := importTimeout
	importTimeout = time.Second
	t.Cleanup(func() { importTimeout = prev })

	is.True(errors.Is(waitImport(t, importAsync(ctx, be, "stalled", admin, remote)), proto.ErrImportFailed))
	assertNothingLeft(t, be, "stalled")
}

// StopImports, which shutdown calls, stops the imports in flight and
// refuses new ones; WaitForImports returns once they have cleaned up.
func TestStopImports(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	remote, _, accepted := stallingRemote(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	errc := importAsync(ctx, be, "stalled", admin, remote)
	waitAccepted(t, accepted)
	be.StopImports()
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	be.WaitForImports(wctx)
	is.NoErr(wctx.Err())
	is.True(errors.Is(waitImport(t, errc), proto.ErrImportFailed))
	assertNothingLeft(t, be, "stalled")

	_, err = be.ImportRepository(ctx, "late", admin, remote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrImportsDisabled))
	assertNothingLeft(t, be, "late")
}

// max_repos_per_user is checked before an import clones anything, against
// the user's repositories. Imports in flight do not count, so one that
// passed that check can still find the limit reached when it records its
// repository, after the clone: it then fails and leaves nothing behind.
func TestImportRepoLimit(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 1
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	// Below the limit, the import gets a slot.
	slot, err := be.reserveImport(ctx, alice, false, true)
	is.NoErr(err)
	defer slot.release()

	// A create takes the last repository while the import clones.
	_, err = be.CreateRepository(ctx, "created", alice, proto.RepositoryOptions{})
	is.NoErr(err)

	imp := importSpec{
		name:      "imported",
		path:      be.repoPath("imported"),
		remote:    src,
		logRemote: src,
		limited:   true,
	}
	is.NoErr(claimRepoPath(imp.path))
	_, err = be.importRepository(ctx, alice, imp)
	is.True(errors.Is(err, proto.ErrRepoLimitReached))
	assertNothingLeft(t, be, "imported")

	// At the limit, the next import is refused before it clones.
	_, err = be.reserveImport(ctx, alice, false, true)
	is.True(errors.Is(err, proto.ErrRepoLimitReached))
}

// reserveImport caps the imports in flight per user, except for server
// admins, and on the whole server, and frees a slot once.
func TestReserveImportCaps(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	var releases []func()
	for range maxUserConcurrentImports {
		slot, err := be.reserveImport(ctx, alice, false, false)
		is.NoErr(err)
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, alice, false, false)
	is.True(errors.Is(err, proto.ErrTooManyImports))
	// Callers without an account count together, apart from alice.
	for range maxUserConcurrentImports {
		slot, err := be.reserveImport(ctx, nil, false, false)
		is.NoErr(err)
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, nil, false, false)
	is.True(errors.Is(err, proto.ErrTooManyImports))

	// Releasing twice frees one slot.
	releases[0]()
	releases[0]()
	slot, err := be.reserveImport(ctx, alice, false, false)
	is.NoErr(err)
	releases[0] = slot.release
	_, err = be.reserveImport(ctx, alice, false, false)
	is.True(errors.Is(err, proto.ErrTooManyImports))

	// Admins are not capped per user, but the server cap holds for them.
	for len(releases) < maxConcurrentImports {
		slot, err := be.reserveImport(ctx, alice, true, false)
		is.NoErr(err)
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, alice, true, false)
	is.True(errors.Is(err, proto.ErrTooManyImports))

	for _, release := range releases {
		release()
	}
	be.imports.mu.Lock()
	is.Equal(be.imports.total, 0)
	is.Equal(len(be.imports.perUser), 0)
	be.imports.mu.Unlock()
}

// An import stopped just after its repository was committed must not report
// failure while leaving that repository behind: registering runs to the
// end, and the import either succeeds or removes what it created.
func TestImportStoppedDuringRegister(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	name := "registered"
	imp := importSpec{
		name:      name,
		path:      be.repoPath(name),
		remote:    src,
		logRemote: src,
	}
	is.NoErr(claimRepoPath(imp.path))
	be.afterRegister = cancel
	defer func() { be.afterRegister = nil }()

	_, err = be.importRepository(ctx, admin, imp)
	if err != nil {
		assertNothingLeft(t, be, name)
		return
	}
	_, err = be.Repository(context.Background(), name)
	is.NoErr(err)
	_, err = os.Stat(imp.path)
	is.NoErr(err)
}
