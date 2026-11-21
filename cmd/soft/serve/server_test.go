package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
	"github.com/charmbracelet/soft-serve/pkg/daemon"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	sshsrv "github.com/charmbracelet/soft-serve/pkg/ssh"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // sqlite driver
)

func setupTestBackend(tb testing.TB) (context.Context, *config.Config, *backend.Backend) {
	tb.Helper()
	ctx, cfg, be, _ := setupTestBackendWithDB(tb)
	return ctx, cfg, be
}

func setupTestBackendWithDB(tb testing.TB) (context.Context, *config.Config, *backend.Backend, *db.DB) {
	tb.Helper()
	is := is.New(tb)

	cfg := config.DefaultConfig()
	cfg.DataPath = tb.TempDir()
	is.NoErr(cfg.Validate())

	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)

	is.NoErr(os.MkdirAll(cfg.DataPath, os.ModePerm))
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	is.NoErr(err)
	tb.Cleanup(func() { dbx.Close() }) //nolint: errcheck

	is.NoErr(migrate.Migrate(ctx, dbx, cfg, nil))

	dbstore := database.New()
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)

	return ctx, cfg, be, dbx
}

func TestWarnIfAnonAdminAccess(t *testing.T) {
	cases := []struct {
		name         string
		allowKeyless bool
		anonAccess   access.AccessLevel
		wantWarning  bool
	}{
		{"defaults: keyless disabled, read-only anon", false, access.ReadOnlyAccess, false},
		{"keyless allowed but anon access below admin", true, access.ReadWriteAccess, false},
		{"admin anon access but keyless disallowed", false, access.AdminAccess, false},
		{"keyless allowed with admin anon access: the dangerous combo", true, access.AdminAccess, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			is := is.New(t)
			ctx, cfg, be := setupTestBackend(t)

			allow := c.allowKeyless
			cfg.AllowKeyless = &allow
			cfg.AnonAccess = &c.anonAccess

			var buf bytes.Buffer
			logger := log.New(&buf)

			warnIfAnonAdminAccess(ctx, be, logger)

			gotWarning := strings.Contains(buf.String(), "WARNING")
			is.Equal(gotWarning, c.wantWarning)
		})
	}
}

// A server with no total time limit is warned about at startup, once per
// enabled server: a config.yaml from before the non-zero default keeps
// max_timeout: 0. A disabled server is not.
func TestWarnUnboundedTimeouts(t *testing.T) {
	is := is.New(t)
	warnings := func(cfg *config.Config) string {
		var buf bytes.Buffer
		warnUnboundedTimeouts(cfg, log.New(&buf))
		return buf.String()
	}

	is.Equal(warnings(config.DefaultConfig()), "")

	cfg := config.DefaultConfig()
	cfg.SSH.MaxTimeout, cfg.Git.MaxTimeout, cfg.HTTP.MaxTimeout = 0, 0, 0
	cfg.Git.Enabled = false
	got := warnings(cfg)
	is.True(strings.Contains(got, "WARN"))
	is.True(strings.Contains(got, "ssh.max_timeout"))
	is.True(strings.Contains(got, "http.max_timeout"))
	is.True(!strings.Contains(got, "git.max_timeout"))
	is.True(strings.Contains(got, "recommended=21600"))
}

func TestEnsureDefaultRepoDisabled(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = ""
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

// With admin-only creation on, the boot-time default repository is still
// created: it is attributed to the lowest-ID admin, who may create.
func TestEnsureDefaultRepoCreatesWhenAdminOnly(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.AdminOnlyCreate = true
	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.Name(), "gitops")
}

// User 1 is not necessarily an admin (it can be demoted, and a database
// imported from an older version keeps that version's user IDs), so the
// default repository goes to the lowest-ID admin rather than to user 1, who
// would otherwise get admin access to it as its owner.
func TestEnsureDefaultRepoOwnedByAdminWhenUserOneDemoted(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	user1, err := be.UserByID(ctx, 1)
	is.NoErr(err)
	// Two more admins, so user 1 can be demoted and the lowest ID wins.
	admin, err := be.CreateUser(ctx, "root2", proto.UserOptions{Admin: true})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "root3", proto.UserOptions{Admin: true})
	is.NoErr(err)
	is.NoErr(be.SetAdmin(ctx, user1.Username(), false))

	cfg.AdminOnlyCreate = true
	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.UserID(), admin.ID())
}

// When user 1 has been deleted, the default repository is still created and
// attributed to the remaining admin.
func TestEnsureDefaultRepoCreatedWhenUserOneDeleted(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	admin, err := be.CreateUser(ctx, "root2", proto.UserOptions{Admin: true})
	is.NoErr(err)
	is.NoErr(be.DeleteUser(ctx, "admin"))

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.UserID(), admin.ID())
}

func TestEnsureDefaultRepoCreatesWhenMissing(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.Name(), "gitops")
	is.Equal(r.IsPrivate(), false)
}

func TestEnsureDefaultRepoIsIdempotent(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	logger := log.New(os.Stderr)

	ensureDefaultRepo(ctx, cfg, be, logger)
	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	createdAt := r.CreatedAt()

	// Restart-safety: a second boot against the same data must not error or
	// touch the existing repository.
	ensureDefaultRepo(ctx, cfg, be, logger)

	r, err = be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.CreatedAt(), createdAt)

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 1)
}

func TestEnsureDefaultRepoInvalidNameSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "../not valid!"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLooksUpExisting(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	owner, err := be.UserByID(ctx, 1)
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "gitops", owner, proto.RepositoryOptions{
		Private: true,
	})
	is.NoErr(err)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.IsPrivate(), true)
}

func TestEnsureDefaultRepoOwnerLookupFailsSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Delete the only admin account so there is no owner to attribute the
	// repository to. ensureDefaultRepo must log and return, not panic.
	is.NoErr(be.DeleteUser(ctx, "admin"))

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLookupErrorSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)

	// Create the repo directory on disk so Repository() passes os.Stat,
	// then close the DB so the lookup behind it fails with a non-not-found error.
	repoDir := filepath.Join(cfg.DataPath, "repos", "gitops.git")
	is.NoErr(os.MkdirAll(repoDir, os.ModePerm))
	is.NoErr(dbx.Close())

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	_, err := be.UserByID(ctx, 1)
	is.True(err != nil) // db is closed, sanity check we broke it correctly
}

func TestEnsureDefaultRepoCreateErrorSkipped(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforceable directory permissions as a non-root user")
	}

	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Make the "repos" directory read-only so CreateRepository's git.Init
	// fails for a reason other than the repo already existing.
	reposDir := filepath.Join(cfg.DataPath, "repos")
	is.NoErr(os.MkdirAll(reposDir, os.ModePerm))
	is.NoErr(os.Chmod(reposDir, 0o555))
	t.Cleanup(func() { os.Chmod(reposDir, 0o755) }) //nolint: errcheck

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

// During shutdown, a second interrupt or terminate signal ends the wait for
// webhook deliveries. SIGHUP comes on another channel (see notifySignals).
func TestAbortOnSignal(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	sigs := make(chan os.Signal)
	abort, stop := abortOnSignal(sigs, log.New(&logs))
	defer stop()
	if abort.Err() != nil {
		t.Fatal("the wait was aborted before a signal")
	}

	select {
	case sigs <- syscall.SIGTERM:
	case <-time.After(time.Minute):
		t.Fatal("the loop did not take SIGTERM")
	}
	select {
	case <-abort.Done():
	case <-time.After(time.Minute):
		t.Fatal("SIGTERM did not abort the wait")
	}
	is.True(strings.Contains(logs.String(), "not waiting for webhook deliveries"))
}

// ensureClientKey creates a missing client key, keeps an existing one, and
// reports a path it cannot check, here because a file stands where its
// directory should be, rather than leave the server without a key.
func TestEnsureClientKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh", "client_ed25519")
	if err := ensureClientKey(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no client key written: %v", err)
	}
	if err := ensureClientKey(path); err != nil {
		t.Fatal(err)
	}
	if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, first) {
		t.Errorf("ensureClientKey replaced the existing key (%v)", err)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureClientKey(filepath.Join(file, "client_ed25519")); err == nil || !strings.Contains(err.Error(), "client ssh key") {
		t.Errorf("ensureClientKey() = %v, want a client key error", err)
	}
}

// newStartTestServer returns a server whose four listeners are on the given
// loopback addresses. It is closed when the test ends.
func newStartTestServer(t *testing.T, sshAddr, gitAddr, httpAddr, statsAddr string) *Server {
	t.Helper()
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	cfg.SSH.ListenAddr = sshAddr
	cfg.Git.ListenAddr = gitAddr
	cfg.HTTP.ListenAddr = httpAddr
	cfg.Stats.ListenAddr = statsAddr

	s, err := NewServer(ctx)
	is.NoErr(err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func loopbackAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(test.RandomPort()))
}

// startAsync runs s.Start and returns a channel that receives its result.
func startAsync(s *Server) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	return errc
}

func waitStart(t *testing.T, errc <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(time.Minute):
		t.Fatalf("Start did not return %s", why)
		return nil
	}
}

// requireBindable fails the test unless addr can be bound, that is, unless
// nothing listens on it any more.
func requireBindable(t *testing.T, addr string) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("%s is still in use: %v", addr, err)
	}
	_ = l.Close()
}

// Start returns at once when one server's port is taken, and leaves none of
// the others listening, instead of running on with only some servers up.
func TestStartFailsWhenAListenerCannotBind(t *testing.T) {
	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close() //nolint: errcheck

	// The SSH and Git servers bind before the HTTP server, whose port is
	// taken.
	sshAddr, gitAddr := loopbackAddr(), loopbackAddr()
	s := newStartTestServer(t, sshAddr, gitAddr, taken.Addr().String(), loopbackAddr())

	err = waitStart(t, startAsync(s), "although the HTTP server could not listen")
	if err == nil || !strings.Contains(err.Error(), "HTTP server") {
		t.Fatalf("Start returned %v, want the HTTP server's listen error", err)
	}
	requireBindable(t, sshAddr)
	requireBindable(t, gitAddr)
}

// Start serves on every enabled listener, and Shutdown makes it return and
// releases all of them.
func TestStartServesAllListenersUntilShutdown(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	errc := startAsync(s)

	for _, addr := range addrs {
		deadline := time.Now().Add(time.Minute)
		for {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr) // one attempt; the loop retries until the deadline
			if err == nil {
				_ = conn.Close()
				break
			}
			select {
			case err := <-errc:
				t.Fatalf("Start returned %v before %s accepted connections", err, addr)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("nothing accepts connections on %s: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Shutdown(ctx, ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := waitStart(t, errc, "after Shutdown"); err != nil {
		t.Fatalf("Start returned %v after Shutdown, want nil", err)
	}
	for _, addr := range addrs {
		requireBindable(t, addr)
	}
}

// A Close that comes before the servers start serving, here even before
// Start, still stops them. Otherwise the SSH server went on to serve after
// Close and Start never returned, and the Git daemon's listener stayed open.
func TestStartAfterCloseDoesNotServe(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitStart(t, startAsync(s), "after Close"); err != nil {
		t.Fatalf("Start returned %v after Close, want nil", err)
	}
	for _, addr := range addrs {
		requireBindable(t, addr)
	}
}

// An enabled server with no listen address is an error rather than a random
// port on every interface.
func TestStartRejectsEmptyListenAddr(t *testing.T) {
	sshAddr := loopbackAddr()
	s := newStartTestServer(t, sshAddr, "", loopbackAddr(), loopbackAddr())
	err := waitStart(t, startAsync(s), "with an empty Git listen address")
	if err == nil || !strings.Contains(err.Error(), "Git daemon: listen address is empty") {
		t.Fatalf("Start returned %v, want an empty listen address error", err)
	}
	requireBindable(t, sshAddr)
}

// With every server disabled, Start fails instead of returning nil at once,
// which made "soft serve" exit successfully without serving anything.
func TestStartFailsWithNoServerEnabled(t *testing.T) {
	s := newStartTestServer(t, loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr())
	s.Config.SSH.Enabled = false
	s.Config.Git.Enabled = false
	s.Config.HTTP.Enabled = false
	s.Config.Stats.Enabled = false
	err := waitStart(t, startAsync(s), "with no server enabled")
	if err == nil || !strings.Contains(err.Error(), "no server is enabled") {
		t.Fatalf("Start returned %v, want a no server enabled error", err)
	}
}

// A Shutdown that comes right after Start bound its listeners, while the
// servers are only starting to serve on them, still stops them all within
// its deadline. The SSH server used to wait for a listener it started serving
// on after its Shutdown ran, and that listener was closed only after every
// server's Shutdown returned, so Shutdown ran into its deadline.
func TestShutdownRightAfterStart(t *testing.T) {
	for i := range 50 {
		addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
		s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
		errc := startAsync(s)

		deadline := time.Now().Add(time.Minute)
		for {
			s.mu.Lock()
			bound := s.listeners != nil
			s.mu.Unlock()
			if bound {
				break
			}
			select {
			case err := <-errc:
				t.Fatalf("run %d: Start returned %v before binding its listeners", i, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %d: Start did not bind its listeners", i)
			}
			runtime.Gosched()
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := s.Shutdown(ctx, ctx)
		cancel()
		if err != nil {
			t.Fatalf("run %d: Shutdown: %v", i, err)
		}
		if err := waitStart(t, errc, "after Shutdown"); err != nil {
			t.Fatalf("run %d: Start returned %v after Shutdown, want nil", i, err)
		}
		for _, addr := range addrs {
			requireBindable(t, addr)
		}
	}
}

// A server that fails while serving makes Start return its error at once.
// Start used to hold the error until every other server had stopped too, so
// the process kept running with only some of its servers up.
func TestStartReturnsWhenAServerFails(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	errc := startAsync(s)

	var httpL *serveListener
	deadline := time.Now().Add(time.Minute)
	for {
		s.mu.Lock()
		if s.listeners != nil {
			httpL = s.listeners[2] // SSH, Git, HTTP, Stats
		}
		s.mu.Unlock()
		if httpL != nil {
			break
		}
		select {
		case err := <-errc:
			t.Fatalf("Start returned %v before binding its listeners", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Start did not bind its listeners")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-httpL.serving:
	case <-time.After(time.Minute):
		t.Fatal("the HTTP server did not start serving")
	}

	// Close the HTTP server's listener behind its back, so that its Serve
	// fails while the other servers keep running.
	_ = httpL.Listener.Close()

	err := waitStart(t, errc, "although the HTTP server failed")
	if err == nil || !strings.Contains(err.Error(), "HTTP server") {
		t.Fatalf("Start returned %v, want the HTTP server's error", err)
	}
}

// requireServerClosed fails the test unless serve, run on a listener that is
// already closed, reports want, which each server returns only once it has
// been closed itself. A server that is still open reports the listener's
// error instead.
func requireServerClosed(t *testing.T, name string, serve func(net.Listener) error, want error) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if err := serve(l); !errors.Is(err, want) {
		t.Errorf("%s: Serve returned %v, want %v: it was not closed", name, err, want)
	}
}

// When NewServer fails, here because the TLS certificate cannot be loaded, it
// closes the servers it already built instead of dropping them.
func TestNewServerClosesWhatItBuiltOnError(t *testing.T) {
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))
	cfg.HTTP.TLSCertPath = filepath.Join(t.TempDir(), "missing.crt")
	cfg.HTTP.TLSKeyPath = filepath.Join(t.TempDir(), "missing.key")

	var closed []*Server
	orig := closeFailedServer
	closeFailedServer = func(s *Server) {
		closed = append(closed, s)
		orig(s)
	}
	t.Cleanup(func() { closeFailedServer = orig })

	s, err := NewServer(ctx)
	if err == nil || !strings.Contains(err.Error(), "create cert reloader") {
		t.Fatalf("NewServer returned %v, want a cert reloader error", err)
	}
	if s != nil {
		t.Fatalf("NewServer returned a server along with its error")
	}
	if len(closed) != 1 {
		t.Fatalf("NewServer closed %d servers, want 1", len(closed))
	}
	// The SSH server is left out: it can serve again after Close, so serving
	// on a closed listener does not tell whether it was closed.
	srv := closed[0]
	requireServerClosed(t, "Git daemon", srv.GitDaemon.Serve, daemon.ErrServerClosed)
	requireServerClosed(t, "HTTP server", srv.HTTPServer.Serve, http.ErrServerClosed)
	requireServerClosed(t, "Stats server", srv.StatsServer.Serve, http.ErrServerClosed)
}

// Close skips the servers that were never built, so that it can close a
// server NewServer failed to finish, here one that got no further than its
// Git daemon.
func TestCloseSkipsServersNotBuilt(t *testing.T) {
	ctx, _, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	sshSrv, err := sshsrv.NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gitDaemon, err := daemon.NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Cron: cron.NewScheduler(ctx), SSHServer: sshSrv, GitDaemon: gitDaemon}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireServerClosed(t, "Git daemon", gitDaemon.Serve, daemon.ErrServerClosed)
}

// Shutdown cancels a running cron job, such as a mirror sync, and waits for
// it to end, so it does not run on against the database the caller closes
// next.
func TestShutdownCancelsAndWaitsForCronJobs(t *testing.T) {
	started := make(chan struct{}, 1)
	// The job holds until it is cancelled, or until the test ends: a
	// Shutdown that does not cancel it runs into its deadline.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var cancelled, finished atomic.Bool
	hook := func(ctx context.Context) {
		select {
		case started <- struct{}{}:
		default:
			return
		}
		select {
		case <-ctx.Done():
			cancelled.Store(true)
		case <-release:
			return
		}
		// Still busy for a moment after the cancellation, as a job
		// stopping its git processes is.
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	}

	s := newStartTestServer(t, loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr())
	if err := s.Cron.AddFunc("test-blocking", "@every 1s", func() { hook(s.jobCtx) }); err != nil {
		t.Fatal(err)
	}
	errc := startAsync(s)
	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("Start returned %v before the job ran", err)
	case <-time.After(time.Minute):
		t.Fatal("the cron job did not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Shutdown(ctx, ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !cancelled.Load() {
		t.Fatal("Shutdown did not cancel the cron job")
	}
	if !finished.Load() {
		t.Fatal("Shutdown returned while the cron job was still running")
	}
	if err := waitStart(t, errc, "after Shutdown"); err != nil {
		t.Fatalf("Start returned %v after Shutdown, want nil", err)
	}
}

// When the drain runs out, Shutdown cuts off what still runs, and the stop
// is not an error: an ordinary stop during a long transfer used to make
// soft serve exit with status 1 (R59, CFG-17).
func TestShutdownCutsOffWhatOutlivesTheDrain(t *testing.T) {
	sshAddr := loopbackAddr()
	s := newStartTestServer(t, sshAddr, loopbackAddr(), loopbackAddr(), loopbackAddr())
	errc := startAsync(s)

	// An authenticated SSH connection with no session: the SSH server
	// keeps it, and so its drain, until the idle timeout.
	closed := dialIdleSSH(t, sshAddr, errc)
	// The drain is over before it starts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Shutdown(ctx, ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil when only the drain ran out", err)
	}
	if err := waitStart(t, errc, "after Shutdown"); err != nil {
		t.Fatalf("Start returned %v after Shutdown, want nil", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Minute):
		t.Fatal("the SSH connection is still open a minute after Shutdown")
	}
}

// dialIdleSSH opens an authenticated SSH connection with no session to the
// server at addr, which Start, reporting on errc, is starting. The SSH
// server holds it, and with it its drain, until its idle timeout. The
// returned channel is closed when the server closes the connection.
func dialIdleSSH(t *testing.T, addr string, errc <-chan error) <-chan struct{} {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var client *gossh.Client
	deadline := time.Now().Add(time.Minute)
	for {
		client, err = gossh.Dial("tcp", addr, &gossh.ClientConfig{
			User:            "test",
			Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
			HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec // test server
			Timeout:         time.Minute,
		})
		if err == nil {
			break
		}
		select {
		case err := <-errc:
			t.Fatalf("Start returned %v before the SSH server accepted connections", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot connect to the SSH server: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = client.Close() })
	closed := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(closed)
	}()
	return closed
}

// Close is Shutdown with contexts that are already done: with a connection
// that would hold a drain until its idle timeout, it returns at once and
// cuts the connection off. It does not warn that a drain ran out, since
// there was none (S14 round-1 review notes 10 and 11).
func TestCloseCutsOffWithoutDraining(t *testing.T) {
	sshAddr := loopbackAddr()
	s := newStartTestServer(t, sshAddr, loopbackAddr(), loopbackAddr(), loopbackAddr())
	var logs syncBuffer
	s.logger = log.New(&logs)
	errc := startAsync(s)
	closed := dialIdleSSH(t, sshAddr, errc)

	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Close still waits a minute later: it drained instead of cutting off")
	}
	if err := waitStart(t, errc, "after Close"); err != nil {
		t.Fatalf("Start returned %v after Close, want nil", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Minute):
		t.Fatal("the SSH connection is still open a minute after Close")
	}
	if out := logs.String(); strings.Contains(out, "stopping with connections still open") {
		t.Errorf("Close warned about a drain it did not run: %s", out)
	}
}

// syncBuffer is a bytes.Buffer safe for the goroutines of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Close, which waits for no delivery, still closes the delivery queue, so
// no new delivery starts: a push afterwards to a repository with a push
// webhook is skipped, not sent (S14 round-2 review note 2).
func TestCloseStopsDeliveries(t *testing.T) {
	// The backend logs to the default logger it finds when it is built.
	old := log.Default()
	t.Cleanup(func() { log.SetDefault(old) })
	var logs syncBuffer
	log.SetDefault(log.New(&logs))

	s := newStartTestServer(t, loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr())
	// The webhook points at a closed loopback port, which the allowlist
	// lets it name; nothing must be sent to it.
	s.Config.Webhook.AllowedHosts = []string{"127.0.0.1"}
	if err := s.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), s.Config)
	owner, err := s.Backend.DefaultAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.Backend.CreateSystemRepository(ctx, "hooked", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Backend.CreateWebhook(ctx, repo, "http://127.0.0.1:1/hook", webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Backend.AfterPush(ctx, repo, owner, []hooks.HookArg{{
		OldSha: strings.Repeat("0", 40), NewSha: strings.Repeat("1", 40), RefName: "refs/heads/main",
	}})
	if out := logs.String(); !strings.Contains(out, "push webhook events skipped: server is shutting down") {
		t.Errorf("a push after Close was not skipped: the delivery queue is open:\n%s", out)
	}
}

// waitAtLeast waits until the caller's deadline, or for at least d when
// that comes sooner or the caller has none, and a cancelled caller does not
// end it (S14 round-1 review note 9).
func TestWaitAtLeast(t *testing.T) {
	far, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx, stop := waitAtLeast(far, time.Millisecond)
	defer stop()
	if dl, _ := ctx.Deadline(); dl.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("deadline %v, want the caller's, an hour away", dl)
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	ctx, stop = waitAtLeast(cancelled, time.Hour)
	defer stop()
	if ctx.Err() != nil {
		t.Error("a cancelled caller ended the wait")
	}
	if dl, ok := ctx.Deadline(); !ok || dl.Before(time.Now().Add(59*time.Minute)) {
		t.Errorf("deadline %v, %v; want an hour away", dl, ok)
	}
}

// NewServer creates the client key, and reads the host key once: a bearer
// request does not read the file, and the key stays the one the SSH server
// serves until a restart (tracker 343(a), CFG-16).
func TestNewServerLoadsKeys(t *testing.T) {
	s := newStartTestServer(t, loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr())
	if _, err := os.Stat(s.Config.SSH.ClientKeyPath); err != nil {
		t.Errorf("no client key: %v", err)
	}
	want, err := config.KeyPair(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.Config.SSH.KeyPath); err != nil {
		t.Fatal(err)
	}
	got, err := config.KeyPair(s.Config)
	if err != nil || got != want {
		t.Errorf("KeyPair() after the key file went = %p, %v; want the key read at start %p", got, err, want)
	}
}

// The README's "Stopping the server" says how long the drain is and that
// the stop succeeds (S14 round-1 review note 12).
func TestReadmeStopMatchesDrain(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(readme), "\r\n", "\n")
	i := strings.Index(doc, "#### Stopping the server")
	if i < 0 {
		t.Fatal("README has no Stopping the server section")
	}
	section := strings.Join(strings.Fields(doc[i:i+strings.Index(doc[i+1:], "\n#")+1]), " ")
	if drainTimeout%time.Second != 0 || backend.ShutdownDeliveryTimeout != time.Minute {
		t.Fatal("the README states the drain in seconds and the delivery wait as a minute")
	}
	for _, want := range []string{
		fmt.Sprintf("%d seconds to finish", drainTimeout/time.Second),
		"exits with status 0",
		"up to a minute more",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("Stopping the server does not say %q:\n%s", want, section)
		}
	}
}
