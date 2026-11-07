package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2/testsession"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite" // sqlite driver
)

func TestSession(t *testing.T) {
	is := is.New(t)
	t.Run("authorized repo access", func(t *testing.T) {
		t.Log("setting up")
		s, close := setup(t)
		s.Stderr = os.Stderr
		t.Log("requesting pty")
		err := s.RequestPty("xterm", 80, 40, nil)
		is.NoErr(err)
		go func() {
			time.Sleep(1 * time.Second)
			// s.Signal(gossh.SIGTERM)
			s.Close() //nolint: errcheck
		}()
		t.Log("waiting for session to exit")
		_, err = s.Output("test")
		var ee *gossh.ExitMissingError
		is.True(errors.As(err, &ee))
		t.Log("session exited")
		is.NoErr(close())
	})
}

func setup(tb testing.TB) (*gossh.Session, func() error) {
	tb.Helper()
	srv, closeDB := setupServer(tb)
	return testsession.New(tb, srv, nil), closeDB
}

// setupServer returns an SSH server running the TUI middleware, and a func
// that closes its database.
func setupServer(tb testing.TB) (*ssh.Server, func() error) {
	tb.Helper()
	is := is.New(tb)
	dp := tb.TempDir()
	is.NoErr(os.Setenv("SOFT_SERVE_DATA_PATH", dp))
	is.NoErr(os.Setenv("SOFT_SERVE_GIT_LISTEN_ADDR", ":9418"))
	is.NoErr(os.Setenv("SOFT_SERVE_SSH_LISTEN_ADDR", fmt.Sprintf("127.0.0.1:%d", test.RandomPort()))) // RandomPort checks only loopback
	tb.Cleanup(func() {
		is.NoErr(os.Unsetenv("SOFT_SERVE_DATA_PATH"))
		is.NoErr(os.Unsetenv("SOFT_SERVE_GIT_LISTEN_ADDR"))
		is.NoErr(os.Unsetenv("SOFT_SERVE_SSH_LISTEN_ADDR"))
		is.NoErr(os.RemoveAll(dp))
	})
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		tb.Fatal(err)
	}
	if err := migrate.Migrate(ctx, dbx); err != nil {
		tb.Fatal(err)
	}
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)
	return &ssh.Server{
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(BubbleTeaMiddleware(func(s ssh.Session) {
			_, _, active := s.Pty()
			if !active {
				os.Exit(1)
			}
			s.Exit(0)
		})),
	}, dbx.Close
}

// TestSetupListensOnLoopback checks that setup configures the SSH server to
// bind only loopback. RandomPort checks that a port is free on loopback
// alone, so a server bound to every interface could collide with a listener
// on another one.
func TestSetupListensOnLoopback(t *testing.T) {
	_, closeDB := setup(t)
	t.Cleanup(func() { _ = closeDB() })
	addr := os.Getenv("SOFT_SERVE_SSH_LISTEN_ADDR")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("invalid SSH listen address %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("SSH listens on %q, want a loopback address", addr)
	}
}

// TestBubbleTeaMiddlewareClosesZoneManager checks that a TUI session that
// ends without the quit key, here by the client dropping the connection,
// still stops its zone manager's worker goroutine.
func TestBubbleTeaMiddlewareClosesZoneManager(t *testing.T) {
	before := zoneWorkers()
	srv, closeDB := setupServer(t)
	t.Cleanup(func() { _ = closeDB() })
	addr := testsession.Listen(t, srv)

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := gossh.NewClientConn(conn, addr, &gossh.ClientConfig{
		User:            "testuser",
		Auth:            []gossh.AuthMethod{gossh.Password("testpass")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	s, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 80, 40, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("test"); err != nil {
		t.Fatal(err)
	}

	waitZoneWorkers(t, "the TUI to start", func(n int) bool { return n > before })
	_ = client.Close()
	waitZoneWorkers(t, "the session's zone manager to stop", func(n int) bool { return n <= before })
}

// waitZoneWorkers waits until ok returns true for the number of running
// bubblezone worker goroutines.
func waitZoneWorkers(t *testing.T, what string, ok func(int) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		n := zoneWorkers()
		if ok(n) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %d zone manager workers running", what, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// zoneWorkers returns the number of running bubblezone worker goroutines.
func zoneWorkers() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "bubblezone/v2.(*Manager).zoneWorker")
		}
		buf = make([]byte, 2*len(buf))
	}
}
