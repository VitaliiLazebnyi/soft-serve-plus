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
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{auth},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
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
			if !closedWithin(c, closed, 10*time.Second) {
				t.Fatal("connection without an account still open after its lifetime")
			}
			if took := time.Since(start); took < lifetime {
				t.Errorf("connection closed after %v, before its lifetime of %v", took, lifetime)
			}
		})
	}

	t.Run("unknown key running a command", func(t *testing.T) {
		c := dialAuth(t, ls.addr, gossh.PublicKeys(newSigner(t)))
		closed := watchClose(c)
		// Spend half the lifetime idle, leaving a wide margin before the
		// command starts.
		if closedWithin(c, closed, lifetime/2) {
			t.Fatal("connection closed before its lifetime")
		}

		// git-upload-pack lists the refs, then waits for the client.
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := s.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := s.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start("git-upload-pack repo1"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(stdout, make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, stdout) //nolint: errcheck
		if closedWithin(c, closed, 2*lifetime) {
			t.Fatal("connection closed while its command ran")
		}

		// A flush packet ends upload-pack.
		if _, err := io.WriteString(stdin, "0000"); err != nil {
			t.Fatal(err)
		}
		if err := s.Wait(); err != nil {
			t.Fatalf("git-upload-pack: %v", err)
		}
		// The clock goes on from where it stopped, with about half the
		// lifetime left, rather than starting again.
		ended := time.Now()
		if !closedWithin(c, closed, lifetime*3/4) {
			t.Fatalf("connection still open %v after its command ended, want the rest of its lifetime", time.Since(ended))
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
			c := dialAuth(t, ls.addr, gossh.PublicKeys(tc.key))
			closed := watchClose(c)
			if out, err := runSession(c, "info"); err != nil && tc.key == ls.alice {
				t.Fatalf("info: %v: %s", err, out)
			}
			if closedWithin(c, closed, lifetime*3/2) {
				t.Fatal("connection closed")
			}
			if _, err := c.NewSession(); err != nil {
				t.Fatalf("new session after the lifetime: %v", err)
			}
		})
	}
}
