package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
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

// dialDaemon connects to the test daemon. The connection is closed when the
// test ends, and reads give up after a deadline so a daemon that never
// answers fails the test instead of hanging it.
func dialDaemon(t *testing.T) net.Conn {
	t.Helper()
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", testDaemon.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	return c
}

// readPktline reads one pkt-line. An ERR packet is returned as a
// *pktline.ErrorLine carrying the daemon's message.
func readPktline(c net.Conn) (string, error) {
	pktout := pktline.NewScanner(c)
	if !pktout.Scan() {
		if err := pktout.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return strings.TrimSpace(string(pktout.Bytes())), nil
}

// wantErrLine fails the test unless err is an ERR packet carrying want.
func wantErrLine(t *testing.T, what string, err error, want error) {
	t.Helper()
	var errLine *pktline.ErrorLine
	if !errors.As(err, &errLine) {
		t.Errorf("%s: expected ERR %q, got %v", what, want, err)
		return
	}
	if errLine.Text != want.Error() {
		t.Errorf("%s: expected ERR %q, got ERR %q", what, want, errLine.Text)
	}
}

// TestIdleTimeout verifies a client that connects and never sends a request
// is told it timed out once the idle timeout passes.
func TestIdleTimeout(t *testing.T) {
	c := dialDaemon(t)
	line, err := readPktline(c)
	if err == nil {
		t.Fatalf("expected a timeout, got line %q", line)
	}
	wantErrLine(t, "idle connection", err, git.ErrTimeout)
}

// requestUploadPack asks the daemon for a repository and returns the first
// line it answers with, or the error it answers with.
func requestUploadPack(t *testing.T, repo string) (string, error) {
	t.Helper()
	c := dialDaemon(t)
	if err := pktline.NewEncoder(c).EncodeString("git-upload-pack /" + repo + ".git\x00"); err != nil {
		t.Fatalf("expected nil, got error: %v", err)
	}
	return readPktline(c)
}

// refAdvertisement matches the first line of an upload-pack reference
// advertisement: an object id, a space, and a ref name (or capabilities^{}
// for an empty repository).
var refAdvertisement = regexp.MustCompile(`^[0-9a-f]{40} \S+\x00`)

// TestInvalidRepo verifies the daemon, which is always anonymous, answers a
// missing repository and a private one identically so neither is revealed,
// and serves a public one.
func TestInvalidRepo(t *testing.T) {
	ctx := context.Background()
	if _, err := testBackend.CreateRepository(ctx, "private", nil, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatalf("failed to create private repo: %v", err)
	}

	if _, err := testBackend.CreateRepository(ctx, "public", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create public repo: %v", err)
	}

	for _, tc := range []struct {
		what, repo string
	}{
		{"missing repo", "test"},
		{"private repo", "private"},
		// Dot-dot segments are resolved inside the repositories
		// directory, so this names the missing repository "escape".
		{"dot-dot path", "../escape"},
		// ... and this one names "private".
		{"dot-dot path to a private repo", "x/../../private"},
	} {
		line, err := requestUploadPack(t, tc.repo)
		if err == nil {
			t.Errorf("%s: expected ERR %q, got line %q", tc.what, git.ErrNotAuthed, line)
			continue
		}
		wantErrLine(t, tc.what, err, git.ErrNotAuthed)
	}

	// A public repository is served: the daemon advertises its references,
	// also when the path reaches it through dot-dot segments.
	for _, repo := range []string{"public", "../public", "x/../../public"} {
		line, err := requestUploadPack(t, repo)
		if err != nil {
			t.Errorf("public repo %q was refused: %v", repo, err)
			continue
		}
		if !refAdvertisement.MatchString(line) {
			t.Errorf("public repo %q: expected a reference advertisement, got %q", repo, line)
		}
	}
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
