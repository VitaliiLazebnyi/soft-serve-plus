package backend

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/proto"
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

// maxStallAttempts bounds how many times startStalledImport starts an
// import whose ssh connect to the stalling remote timed out.
const maxStallAttempts = 4

// startStalledImport starts importing remote, a stallingRemote, as user, and
// waits for the import to reach the remote. It returns the import's error
// channel (see importAsync).
//
// An import that ends before it reaches the remote fails the test with what
// the backend logged, since ImportRepository itself returns only the fixed
// proto.ErrImportFailed. One whose ssh connect to the remote timed out is
// started again, up to maxStallAttempts in all: on a loaded macOS machine
// the kernel can stall a loopback connect until it times out (KNOWN_ISSUES
// item 309), and such a connect never reached the remote, so the test has
// not started. Every other early end fails the test.
func startStalledImport(t *testing.T, ctx context.Context, be *Backend, logs *syncBuffer, name string, user proto.User, remote string, accepted <-chan struct{}) <-chan error {
	t.Helper()
	for attempt := 1; ; attempt++ {
		before := len(logs.String())
		errc := importAsync(ctx, be, name, user, remote)
		select {
		case <-accepted:
			return errc
		case err := <-errc:
			logged := logs.String()[before:]
			if attempt < maxStallAttempts && strings.Contains(logged, "ssh: connect to host") &&
				strings.Contains(logged, "Operation timed out") {
				t.Logf("the ssh connect to the stalling remote timed out (attempt %d of %d), starting the import again:\n%s",
					attempt, maxStallAttempts, logged)
				continue
			}
			t.Fatalf("the import ended before it reached the remote: %v; the backend logged:\n%s", err, logged)
		case <-time.After(time.Minute):
			t.Fatal("the import never reached the remote")
		}
		return nil
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
	_, err := be.Repository(context.Background(), name)
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = os.Stat(be.repoPath(name))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not true: errors.Is(err, os.ErrNotExist)")
	}
}

// An import stops, and cleans up, when the caller goes away: a dropped SSH
// session must not leave a clone running on the server.
func TestImportRepositoryStopsWithCaller(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	remote, _, accepted := stallingRemote(t)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	errc := startStalledImport(t, ctx, be, &logs, "stalled", admin, remote, accepted)
	cancel()
	if !errors.Is(waitImport(t, errc), proto.ErrImportFailed) {
		t.Fatal("not true: errors.Is(waitImport(t, errc), proto.ErrImportFailed)")
	}
	assertNothingLeft(t, be, "stalled")
}

// An import that runs past importTimeout is stopped and cleaned up.
func TestImportRepositoryTimeout(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	remote, _, _ := stallingRemote(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	be.importTimeout = time.Second

	if !errors.Is(waitImport(t, importAsync(ctx, be, "stalled", admin, remote)), proto.ErrImportFailed) {
		t.Fatal("not true: errors.Is(waitImport(t, importAsync(ctx, be, \"stalled\", admin, remote)), proto.ErrImportFailed)")
	}
	assertNothingLeft(t, be, "stalled")
}

// StopImports, which shutdown calls, stops the imports in flight and
// refuses new ones; WaitForImports returns once they have cleaned up.
func TestStopImports(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be, cfg := newTestBackendWithLog(t, &logs)
	remote, _, accepted := stallingRemote(t)
	ctx := backendTestContext(be, cfg)
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	errc := startStalledImport(t, ctx, be, &logs, "stalled", admin, remote, accepted)
	be.StopImports()
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	be.WaitForImports(wctx)
	if err := wctx.Err(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(waitImport(t, errc), proto.ErrImportFailed) {
		t.Fatal("not true: errors.Is(waitImport(t, errc), proto.ErrImportFailed)")
	}
	assertNothingLeft(t, be, "stalled")

	_, err = be.ImportRepository(ctx, "late", admin, remote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrImportsDisabled) {
		t.Fatal("not true: errors.Is(err, proto.ErrImportsDisabled)")
	}
	assertNothingLeft(t, be, "late")
}

// max_repos_per_user is checked before an import clones anything, against
// the user's repositories. Imports in flight do not count, so one that
// passed that check can still find the limit reached when it records its
// repository, after the clone: it then fails and leaves nothing behind.
func TestImportRepoLimit(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 1
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if out, err := exec.CommandContext(context.Background(), "git", "init", "--bare", "--quiet", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	// Below the limit, the import gets a slot.
	slot, err := be.reserveImport(ctx, alice, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer slot.release()

	// A create takes the last repository while the import clones.
	_, err = be.CreateRepository(ctx, "created", alice, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	imp := importSpec{
		name:      "imported",
		path:      be.repoPath("imported"),
		remote:    src,
		logRemote: src,
		limited:   true,
	}
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	_, err = be.importRepository(ctx, alice, imp)
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
	assertNothingLeft(t, be, "imported")

	// At the limit, the next import is refused before it clones.
	_, err = be.reserveImport(ctx, alice, false, true)
	if !errors.Is(err, proto.ErrRepoLimitReached) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoLimitReached)")
	}
}

// reserveImport caps the imports in flight per user, except for server
// admins, and on the whole server, and frees a slot once.
func TestReserveImportCaps(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var releases []func()
	for range maxUserConcurrentImports {
		slot, err := be.reserveImport(ctx, alice, false, false)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, alice, false, false)
	if !errors.Is(err, proto.ErrTooManyImports) {
		t.Fatal("not true: errors.Is(err, proto.ErrTooManyImports)")
	}
	// Callers without an account count together, apart from alice.
	for range maxUserConcurrentImports {
		slot, err := be.reserveImport(ctx, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, nil, false, false)
	if !errors.Is(err, proto.ErrTooManyImports) {
		t.Fatal("not true: errors.Is(err, proto.ErrTooManyImports)")
	}

	// Releasing twice frees one slot.
	releases[0]()
	releases[0]()
	slot, err := be.reserveImport(ctx, alice, false, false)
	if err != nil {
		t.Fatal(err)
	}
	releases[0] = slot.release
	_, err = be.reserveImport(ctx, alice, false, false)
	if !errors.Is(err, proto.ErrTooManyImports) {
		t.Fatal("not true: errors.Is(err, proto.ErrTooManyImports)")
	}

	// Admins are not capped per user, but the server cap holds for them.
	for len(releases) < maxConcurrentImports {
		slot, err := be.reserveImport(ctx, alice, true, false)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, slot.release)
	}
	_, err = be.reserveImport(ctx, alice, true, false)
	if !errors.Is(err, proto.ErrTooManyImports) {
		t.Fatal("not true: errors.Is(err, proto.ErrTooManyImports)")
	}

	for _, release := range releases {
		release()
	}
	be.imports.mu.Lock()
	if got := be.imports.total; got != 0 {
		t.Fatalf("be.imports.total = %v, want %v", got, 0)
	}
	if got := len(be.imports.perUser); got != 0 {
		t.Fatalf("len(be.imports.perUser) = %v, want %v", got, 0)
	}
	be.imports.mu.Unlock()
}

// An import stopped just after its repository was committed must not report
// failure while leaving that repository behind: registering runs to the
// end, and the import either succeeds or removes what it created.
func TestImportStoppedDuringRegister(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx, cancel := context.WithCancel(backendTestContext(be, cfg))
	defer cancel()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

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
	if err := claimRepoPath(imp.path); err != nil {
		t.Fatal(err)
	}
	be.afterRegister = cancel
	defer func() { be.afterRegister = nil }()

	_, err = be.importRepository(ctx, admin, imp)
	if err != nil {
		assertNothingLeft(t, be, name)
		return
	}
	_, err = be.Repository(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(imp.path)
	if err != nil {
		t.Fatal(err)
	}
}
