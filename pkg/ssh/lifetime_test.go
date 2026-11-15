package ssh

import (
	"net"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	gossh "golang.org/x/crypto/ssh"
)

// startLifetimeServer starts a server built by NewSSHServer whose
// connections without an account last lifetime instead of
// noAccountLifetime, and returns its address and the user "alice" it holds.
func startLifetimeServer(t *testing.T, lifetime time.Duration) (string, gossh.Signer) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	s, err := NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.srv.ConnCallback = connCallback(s.limiter, log.Default(), lifetime)
	alice := newSigner(t)
	if _, err := backend.FromContext(ctx).CreateUser(ctx, "alice", proto.UserOptions{
		PublicKeys: []gossh.PublicKey{alice.PublicKey()},
	}); err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String(), alice
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

// closedWithin reports whether the server closes c within d, while the
// client keeps the connection busy with keepalives.
func closedWithin(c *gossh.Client, d time.Duration) bool {
	closed := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(closed)
	}()
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

// A connection whose key no account owns, or that has no key, is closed
// once its lifetime has passed, however busy; one whose session runs as an
// account stays open.
func TestNoAccountLifetime(t *testing.T) {
	const lifetime = 500 * time.Millisecond
	addr, alice := startLifetimeServer(t, lifetime)

	for _, tc := range []struct {
		name string
		auth gossh.AuthMethod
	}{
		{"unknown key", gossh.PublicKeys(newSigner(t))},
		{"keyless", gossh.KeyboardInteractive(noAnswers)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			c := dialAuth(t, addr, tc.auth)
			// An anonymous session does not extend the lifetime.
			if _, err := runSession(c, "info"); err != nil {
				t.Logf("info: %v", err)
			}
			if !closedWithin(c, 10*time.Second) {
				t.Fatal("connection without an account still open after its lifetime")
			}
			if took := time.Since(start); took < lifetime {
				t.Errorf("connection closed after %v, before its lifetime of %v", took, lifetime)
			}
		})
	}

	t.Run("account", func(t *testing.T) {
		c := dialAuth(t, addr, gossh.PublicKeys(alice))
		if out, err := runSession(c, "info"); err != nil {
			t.Fatalf("info: %v: %s", err, out)
		}
		if closedWithin(c, 4*lifetime) {
			t.Fatal("connection of an account closed")
		}
		if _, err := runSession(c, "info"); err != nil {
			t.Fatalf("info after the lifetime: %v", err)
		}
	})
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
