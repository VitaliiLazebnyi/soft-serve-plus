package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
	dto "github.com/prometheus/client_model/go"
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
	srv, closeDB := newTestTUIServer(tb)
	return testsession.New(tb, srv, nil), closeDB
}

// newTestTUIServer returns a server that runs the TUI for every session, and
// a function closing its database.
func newTestTUIServer(tb testing.TB) (*ssh.Server, func() error) {
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
		Handler: ContextMiddleware(cfg, dbx, dbstore, be, log.Default())(TUIMiddleware(func(s ssh.Session) {
			_, _, active := s.Pty()
			if !active {
				os.Exit(1)
			}
			s.Exit(0) //nolint: errcheck
		})),
	}, dbx.Close
}

// The time spent in the TUI is counted when the TUI session ends, not when
// the connection it ran on closes.
func TestTUISessionDurationCountedAtSessionEnd(t *testing.T) {
	srv, closeDB := newTestTUIServer(t)
	t.Cleanup(func() { _ = closeDB() })
	addr := testsession.Listen(t, srv)

	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	before := tuiSeconds(t)
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	// Quit the TUI, retrying until the program reads the key.
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
wait:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			break wait
		case <-tick.C:
			_, _ = io.WriteString(stdin, "q")
		case <-timeout:
			t.Fatal("TUI did not quit")
		}
	}

	// The connection is still open.
	if after := tuiSeconds(t); after <= before {
		t.Fatalf("tui_session_seconds_total = %v after the session ended, want more than %v", after, before)
	}
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

func tuiSeconds(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := tuiSessionDuration.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
