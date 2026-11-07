package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestKeepAlive(t *testing.T) {
	for _, c := range []struct {
		idle time.Duration
		want int
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Second, 1},
		{2 * time.Second, 1},
		{3 * time.Second, 1},
		{10 * time.Second, 5},
		{600 * time.Second, 300},
	} {
		if got := keepAlive(c.idle); got != c.want {
			t.Errorf("keepAlive(%v) = %d, want %d", c.idle, got, c.want)
		}
	}
}

// ensureRepo creates the public repository name unless it exists, as it
// does when the tests run more than once.
func ensureRepo(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testBackend.Repository(ctx, name); err == nil {
		return
	}
	if _, err := testBackend.CreateRepository(ctx, name, nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
}

// runGit runs git with args in dir and fails the test if it fails.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestQuietCloneSlowPack clones quietly from a repository whose pack takes
// longer than the idle timeout to start. Neither side talks meanwhile, so
// the daemon has upload-pack send keepalives; without them the connection
// was closed as idle, and the ERR packet written after it broke the clone
// with "bad band".
func TestQuietCloneSlowPack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pack-objects hook needs a POSIX shell")
	}
	ctx := context.Background()
	const name = "slowpack"
	ensureRepo(t, name)
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "README")
	runGit(t, src, "commit", "-q", "-m", "init")
	bare := filepath.Join(testDaemon.cfg.DataPath, "repos", name+".git")
	head := runGit(t, bare, "symbolic-ref", "HEAD")
	// Fetch into the repository rather than push, which would run the
	// server's hooks.
	runGit(t, bare, "fetch", "-q", src, "+HEAD:"+head)

	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 2
	dctx := config.WithContext(context.Background(), &cfg)
	dctx = backend.WithContext(dctx, testBackend)
	d, err := NewGitDaemon(dctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })

	// Delay the pack past the idle timeout. The hook is only honoured
	// from protected configuration, which the command-line scope of
	// GIT_CONFIG_COUNT is; the daemon passes its environment to git.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "uploadpack.packObjectsHook")
	t.Setenv("GIT_CONFIG_VALUE_0", "sleep 3;")

	dst := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "-q", "git://"+l.Addr().String()+"/"+name, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "README")); err != nil {
		t.Errorf("clone is missing README: %v", err)
	}
}

// TestNoErrAfterOutput leaves a request idle after upload-pack answered with
// its references. The daemon closes the connection, but must not write an
// ERR packet: the client already reads the response, so it would take the
// packet for part of it.
func TestNoErrAfterOutput(t *testing.T) {
	const name = "idleafteradvert"
	ensureRepo(t, name)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", testDaemon.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := pktline.NewEncoder(c).EncodeString("git-upload-pack /" + name + ".git\x00"); err != nil {
		t.Fatal(err)
	}
	// Stay silent until the daemon gives up on the connection, then
	// collect everything it sent.
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected a response, got nothing")
	}
	if strings.Contains(string(out), "ERR ") {
		t.Errorf("response has an ERR packet after output started:\n%q", out)
	}
}
