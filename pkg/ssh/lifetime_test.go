package ssh

import (
	"io"
	"net"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	gossh "golang.org/x/crypto/ssh"
)

// lifetimeServer is a server for the lifetime tests.
type lifetimeServer struct {
	addr  string
	alice gossh.Signer // the key of the user alice
	admin gossh.Signer // an admin key no account owns
}

// startLifetimeServer starts a server built by NewSSHServer whose
// connections without an account have lifetime instead of
// noAccountLifetime. It holds the user alice, a listed admin key that no
// account owns, and the public repository repo1.
func startLifetimeServer(t *testing.T, lifetime time.Duration) lifetimeServer {
	t.Helper()
	ls := lifetimeServer{alice: newSigner(t), admin: newSigner(t)}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.InitialAdminKeys = []string{sshutils.MarshalAuthorizedKey(ls.admin.PublicKey())}
	ctx := serverContext(t, cfg, log.Default())
	s, err := NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.srv.ConnCallback = connCallback(s.limiter, log.Default(), lifetime)

	be := backend.FromContext(ctx)
	if _, err := be.CreateUser(ctx, "alice", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{ls.alice.PublicKey()},
	}); err != nil {
		t.Fatal(err)
	}
	// The listed admin key went to the admin account; take it off, so it
	// stays an admin key with no account.
	owner, err := be.UserByPublicKey(ctx, ls.admin.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RemovePublicKey(ctx, owner.Username(), ls.admin.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "repo1", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}

	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	ls.addr = l.Addr().String()
	return ls
}

// dialAuth connects to addr with auth.
func dialAuth(t *testing.T, addr string, auth gossh.AuthMethod) *gossh.Client {
	t.Helper()
	c, err := tryDialAuth(t, addr, auth)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// tryDialAuth is dialAuth returning the error, for a client that may be
// closed by a short lifetime before it is done.
func tryDialAuth(t *testing.T, addr string, auth gossh.AuthMethod) (*gossh.Client, error) {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{auth},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, nil
}

// alive reports whether the server still answers on c.
func alive(c *gossh.Client) bool {
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// lifetimePassed waits until a connection without an account, opened now,
// has been closed by its lifetime. A connection whose lifetime clock started
// earlier and ran all along has then been closed too, whatever the
// machine's load; one still open had its clock stopped.
func lifetimePassed(t *testing.T, ls lifetimeServer) {
	t.Helper()
	later := dialAuth(t, ls.addr, gossh.PublicKeys(newSigner(t)))
	if !closedWithin(later, watchClose(later), sessionLoopsWait) {
		t.Fatalf("connection without an account still open %s after it opened", sessionLoopsWait)
	}
}

// watchClose returns a channel closed once the server has closed c.
func watchClose(c *gossh.Client) <-chan struct{} {
	closed := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(closed)
	}()
	return closed
}

// closedWithin reports whether closed is closed within d, while the client
// keeps the connection busy with keepalives.
func closedWithin(c *gossh.Client, closed <-chan struct{}, d time.Duration) bool {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(d)
	for {
		select {
		case <-closed:
			return true
		case <-timeout:
			return false
		case <-tick.C:
			go c.SendRequest("keepalive@openssh.com", true, nil) //nolint: errcheck
		}
	}
}

// runSession runs command in a new session on c and returns its output.
func runSession(c *gossh.Client, command string) ([]byte, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close() //nolint: errcheck
	return s.CombinedOutput(command)
}

// The lifetime of a connection without an account is spent only while no
// command runs, and closes the connection however busy it keeps it. A
// connection with an account, or with an admin key, has no lifetime.
//
// The test sees the lifetime pass through connections closed, not through
// the time taken: a client slowed past the short lifetime before it is done
// tries again, and that a stopped clock goes on with the time it had left is
// TestConnLifetimeResumes' check.
func TestNoAccountLifetime(t *testing.T) {
	const lifetime = 2 * time.Second
	ls := startLifetimeServer(t, lifetime)

	for _, tc := range []struct {
		name string
		auth gossh.AuthMethod
	}{
		{"unknown key", gossh.PublicKeys(newSigner(t))},
		{"keyless", gossh.KeyboardInteractive(noAnswers)},
	} {
		t.Run(tc.name+" idle", func(t *testing.T) {
			start := time.Now()
			c := dialAuth(t, ls.addr, tc.auth)
			closed := watchClose(c)
			// An anonymous session does not keep the connection open.
			if _, err := runSession(c, "info"); err != nil {
				t.Logf("info: %v", err)
			}
			if !closedWithin(c, closed, sessionLoopsWait) {
				t.Fatal("connection without an account still open after its lifetime")
			}
			if took := time.Since(start); took < lifetime {
				t.Errorf("connection closed after %v, before its lifetime of %v", took, lifetime)
			}
		})
	}

	t.Run("unknown key running a command", func(t *testing.T) {
		// git-upload-pack lists the refs, then waits for the client. The
		// command must start within the lifetime: a client slowed past it
		// is closed first, and tries again.
		var (
			c     *gossh.Client
			s     *gossh.Session
			stdin io.WriteCloser
		)
		waitFor(t, "a command to start within the lifetime", func() bool {
			var err error
			if c, err = tryDialAuth(t, ls.addr, gossh.PublicKeys(newSigner(t))); err != nil {
				t.Logf("dial: %v", err)
				return false
			}
			if s, err = c.NewSession(); err != nil {
				t.Logf("session: %v", err)
				return false
			}
			stdout, err := s.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if stdin, err = s.StdinPipe(); err != nil {
				t.Fatal(err)
			}
			if err := s.Start("git-upload-pack repo1"); err != nil {
				t.Logf("start: %v", err)
				return false
			}
			if _, err := io.ReadFull(stdout, make([]byte, 4)); err != nil {
				t.Logf("read: %v", err)
				return false
			}
			go io.Copy(io.Discard, stdout) //nolint: errcheck
			return true
		})
		closed := watchClose(c)

		lifetimePassed(t, ls)
		if !alive(c) {
			t.Fatal("connection closed while its command ran")
		}

		// A flush packet ends upload-pack, and the clock goes on.
		if _, err := io.WriteString(stdin, "0000"); err != nil {
			t.Fatal(err)
		}
		if err := s.Wait(); err != nil {
			t.Fatalf("git-upload-pack: %v", err)
		}
		if !closedWithin(c, closed, sessionLoopsWait) {
			t.Fatal("connection still open after its command ended and its lifetime passed")
		}
	})

	for _, tc := range []struct {
		name string
		key  gossh.Signer
	}{
		{"account", ls.alice},
		{"admin key without an account", ls.admin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The connection's first session, which finds the account,
			// must run within the lifetime: a client slowed past it is
			// closed first, and tries again.
			var c *gossh.Client
			waitFor(t, "a session within the lifetime", func() bool {
				var err error
				if c, err = tryDialAuth(t, ls.addr, gossh.PublicKeys(tc.key)); err != nil {
					t.Logf("dial: %v", err)
					return false
				}
				out, err := runSession(c, "info")
				if !alive(c) {
					t.Logf("closed before its first session ran: %v", err)
					return false
				}
				if err != nil && tc.key == ls.alice {
					t.Fatalf("info: %v: %s", err, out)
				}
				return true
			})

			lifetimePassed(t, ls)
			if _, err := c.NewSession(); err != nil {
				t.Fatalf("new session after the lifetime: %v", err)
			}
		})
	}
}

// TestConnLifetimeResumes checks that the lifetime clock stops while a
// command runs and, once it has ended, goes on with the time it had left
// rather than starting again. The clock of an hour-long lifetime is wound
// back to a millisecond before its end instead of waited on, so the
// lifetime is over within the test's wait only if the clock goes on: one
// that started again would run for another hour.
func TestConnLifetimeResumes(t *testing.T) {
	const lifetime = time.Hour
	var l connLifetime
	expired := make(chan struct{})
	l.start(lifetime, func() { close(expired) })
	defer l.stop()
	left := func() time.Duration {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.left
	}

	// Spend all but a millisecond of the lifetime.
	l.mu.Lock()
	l.since = l.since.Add(-(lifetime - time.Millisecond))
	l.mu.Unlock()

	l.commandStarted()
	stopped := left()
	if stopped > time.Millisecond {
		t.Fatalf("time left once a command started = %v, want at most 1ms", stopped)
	}
	// While a second command still runs, the first one ending does not
	// start the clock.
	l.commandStarted()
	l.commandEnded()
	if got := left(); got != stopped {
		t.Fatalf("time left while a second command runs = %v, want %v", got, stopped)
	}

	l.commandEnded()
	select {
	case <-expired:
	case <-time.After(time.Minute):
		t.Fatalf("lifetime not over a minute after the commands ended with %v of it left; the clock started again", stopped)
	}
}
