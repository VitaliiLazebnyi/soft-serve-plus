package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	_ "modernc.org/sqlite" // sqlite driver
)

var (
	testDaemon  *GitDaemon
	testBackend *backend.Backend
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "soft-serve-test")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	// Each request in these tests closes its connection, but the daemon notices
	// asynchronously; leave headroom so back-to-back requests are not refused
	// with "too many connections" on slow (-race) runners.
	cfg.Git.MaxConnections = 10
	cfg.Git.MaxTimeout = 100
	cfg.Git.IdleTimeout = 1
	cfg.Git.ListenAddr = fmt.Sprintf("127.0.0.1:%d", test.RandomPort()) // RandomPort checks only loopback
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		log.Fatal(err)
	}
	defer dbx.Close() //nolint: errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		log.Fatal(err)
	}
	datastore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, datastore)
	be := backend.New(ctx, cfg, dbx, datastore)
	testBackend = be
	ctx = backend.WithContext(ctx, be)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		log.Fatal(err)
	}
	testDaemon = d
	go d.ListenAndServe() //nolint:errcheck
	code := m.Run()
	os.Unsetenv("SOFT_SERVE_DATA_PATH")
	os.Unsetenv("SOFT_SERVE_GIT_MAX_CONNECTIONS")
	os.Unsetenv("SOFT_SERVE_GIT_MAX_TIMEOUT")
	os.Unsetenv("SOFT_SERVE_GIT_IDLE_TIMEOUT")
	os.Unsetenv("SOFT_SERVE_GIT_LISTEN_ADDR")
	_ = d.Close()
	_ = dbx.Close()
	os.Exit(code)
}

func TestIdleTimeout(t *testing.T) {
	var err error
	var c net.Conn
	var tries int
	var dialer net.Dialer
	for {
		c, err = dialer.DialContext(t.Context(), "tcp", testDaemon.addr)
		if err != nil && tries >= 3 {
			t.Fatalf("failed to connect to daemon after %d tries: %v", tries, err)
		}
		tries++
		if testDaemon.conns.Size() != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	_, err = readPktline(c)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
}

// requestUploadPack asks the daemon for a repository and returns the first
// line it answers with.
func requestUploadPack(t *testing.T, repo string) string {
	t.Helper()
	c, err := net.Dial("tcp", testDaemon.addr) //nolint:noctx
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := pktline.NewEncoder(c).EncodeString("git-upload-pack /" + repo + ".git\x00"); err != nil {
		t.Fatalf("expected nil, got error: %v", err)
	}
	// The scanner reports an ERR packet as an error carrying its message.
	line, err := readPktline(c)
	if err != nil {
		return err.Error()
	}
	return line
}

// TestInvalidRepo verifies the daemon, which is always anonymous, answers a
// missing repository and a private one identically so neither is revealed.
func TestInvalidRepo(t *testing.T) {
	ctx := context.Background()
	if _, err := testBackend.CreateRepository(ctx, "private", nil, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatalf("failed to create private repo: %v", err)
	}

	if _, err := testBackend.CreateRepository(ctx, "public", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create public repo: %v", err)
	}

	want := git.ErrNotAuthed.Error()
	missing := requestUploadPack(t, "test")
	private := requestUploadPack(t, "private")
	if missing != want {
		t.Errorf("missing repo: expected %q, got %q", want, missing)
	}
	if private != want {
		t.Errorf("private repo: expected %q, got %q", want, private)
	}

	// A path that escapes the repositories directory gets the same answer.
	if got := requestUploadPack(t, "../escape"); got != want {
		t.Errorf("escaping path: expected %q, got %q", want, got)
	}

	// A public repository is served: the first line is not a refusal.
	public := requestUploadPack(t, "public")
	if public == want || public == git.ErrInvalidRepo.Error() {
		t.Errorf("public repo was refused: %q", public)
	}
}

func readPktline(c net.Conn) (string, error) {
	pktout := pktline.NewScanner(c)
	if !pktout.Scan() {
		return "", pktout.Err()
	}
	return strings.TrimSpace(string(pktout.Bytes())), nil
}

// Serve and Shutdown can run at the same time: Serve either sees the daemon
// closed or is waited for. Serve used to start counting on the WaitGroup
// that Shutdown might already be waiting on, which the race detector reports.
func TestServeRacingShutdown(t *testing.T) {
	ctx := config.WithContext(context.Background(), testDaemon.cfg)
	ctx = backend.WithContext(ctx, testBackend)
	for i := range 100 {
		d, err := NewGitDaemon(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- d.Serve(l) }()

		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = d.Shutdown(sctx)
		cancel()
		if err != nil {
			t.Fatalf("run %d: Shutdown: %v", i, err)
		}
		_ = l.Close() // Serve may not have registered it before Shutdown
		select {
		case err := <-served:
			if !errors.Is(err, ErrServerClosed) {
				t.Fatalf("run %d: Serve returned %v, want ErrServerClosed", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: Serve did not return after Shutdown", i)
		}
	}
}

// TestListensOnLoopback checks that the test daemon binds only loopback.
// RandomPort checks that a port is free on loopback alone, so a daemon bound
// to every interface could collide with a listener on another one.
func TestListensOnLoopback(t *testing.T) {
	host, _, err := net.SplitHostPort(testDaemon.addr)
	if err != nil {
		t.Fatalf("invalid daemon address %q: %v", testDaemon.addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("daemon listens on %q, want a loopback address", testDaemon.addr)
	}
}

// startLimitedDaemon serves a daemon allowing limit connections that never
// time out, and returns it with its address.
func startLimitedDaemon(t *testing.T, limit int) (*GitDaemon, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = testDaemon.cfg.DataPath
	cfg.Git.MaxConnections = limit
	cfg.Git.MaxTimeout = 0
	cfg.Git.IdleTimeout = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = backend.WithContext(ctx, testBackend)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(l) }()
	t.Cleanup(func() {
		_ = d.Close()
		if err := <-served; !errors.Is(err, ErrServerClosed) {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	})
	return d, l.Addr().String()
}

// refused dials addr and reports whether the daemon turns the connection away
// as over the limit. A connection it keeps open sends nothing, so it counts as
// kept once wait passes without an answer.
func refused(t *testing.T, addr string, wait time.Duration) (bool, net.Conn) {
	t.Helper()
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(wait))
	_, err = readPktline(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, c
	}
	if err == nil || err.Error() != git.ErrMaxConnections.Error() {
		t.Fatalf("got %v, want %q or no answer", err, git.ErrMaxConnections)
	}
	return true, c
}

// TestMaxConnections checks that max_connections N lets N connections in and
// refuses the next. It used to let only N-1 in, and a limit of 1 none.
func TestMaxConnections(t *testing.T) {
	for _, limit := range []int{1, 3} {
		d, addr := startLimitedDaemon(t, limit)
		for i := range limit {
			if r, _ := refused(t, addr, 200*time.Millisecond); r {
				t.Fatalf("limit %d: connection %d refused", limit, i+1)
			}
		}
		if r, _ := refused(t, addr, 5*time.Second); !r {
			t.Fatalf("limit %d: connection %d kept, want it refused", limit, limit+1)
		}
		if got := d.conns.Size(); got != limit {
			t.Fatalf("limit %d: %d connections open", limit, got)
		}
	}
}

// TestMaxConnectionsBurst dials many connections at once: exactly limit of
// them are kept. Connections used to be counted in their own goroutines, so
// a burst passed the check before any was counted.
func TestMaxConnectionsBurst(t *testing.T) {
	const limit, dials = 3, 64
	d, addr := startLimitedDaemon(t, limit)
	// Each refused connection gets an answer; wait long enough for all of
	// them before counting the silent ones as kept.
	type result struct {
		c       net.Conn
		err     error
		refused bool
	}
	results := make(chan result, dials)
	for range dials {
		go func() {
			var dialer net.Dialer
			c, err := dialer.DialContext(t.Context(), "tcp", addr)
			if err != nil {
				results <- result{err: err}
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err = readPktline(c)
			results <- result{c: c, refused: err != nil && err.Error() == git.ErrMaxConnections.Error()}
		}()
	}
	var kept int
	for range dials {
		r := <-results
		if r.err != nil {
			t.Fatalf("dial: %v", r.err)
		}
		t.Cleanup(func() { _ = r.c.Close() })
		if !r.refused {
			kept++
		}
	}
	if kept != limit {
		t.Errorf("%d of %d connections kept, want %d", kept, dials, limit)
	}
	if got := d.conns.Size(); got != limit {
		t.Errorf("%d connections open, want %d", got, limit)
	}
}

// TestConnectionsAddAfterCloseAll checks that a connection accepted while the
// daemon closes is not added after CloseAll and so escapes the forced close.
func TestConnectionsAddAfterCloseAll(t *testing.T) {
	m := connections{m: make(map[net.Conn]struct{})}
	a, b := net.Pipe()
	defer a.Close() //nolint:errcheck
	defer b.Close() //nolint:errcheck
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(a, 10); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Add after CloseAll = %v, want ErrServerClosed", err)
	}
	if got := m.Size(); got != 0 {
		t.Fatalf("%d connections after CloseAll, want 0", got)
	}
}
