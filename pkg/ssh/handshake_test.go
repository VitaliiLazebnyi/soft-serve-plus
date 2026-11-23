package ssh

import (
	"net"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	gossh "golang.org/x/crypto/ssh"
)

// NewServer bounds the handshake with config.SSHHandshakeTimeout and
// the sessions per connection with config.SSHMaxSessions: one connection
// opens that many sessions, and the next is refused.
func TestSSHServerFixedLimits(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := serverContext(t, cfg, log.Default())
	s, err := newTestSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.srv.HandshakeTimeout != config.SSHHandshakeTimeout || config.SSHHandshakeTimeout <= 0 {
		t.Errorf("handshake timeout = %s, want config.SSHHandshakeTimeout (%s)", s.srv.HandshakeTimeout, config.SSHHandshakeTimeout)
	}
	if config.SSHMaxSessions <= 0 {
		t.Fatalf("config.SSHMaxSessions = %d, want a limit", config.SSHMaxSessions)
	}

	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	c, err := dialClient(t, l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	for i := range config.SSHMaxSessions {
		sess, err := c.NewSession()
		if err != nil {
			t.Fatalf("session %d of %d: %v", i+1, config.SSHMaxSessions, err)
		}
		defer sess.Close() //nolint: errcheck
	}
	if sess, err := c.NewSession(); err == nil {
		_ = sess.Close()
		t.Fatalf("session %d on one connection accepted, want refused", config.SSHMaxSessions+1)
	} else if oce, ok := err.(*gossh.OpenChannelError); !ok || oce.Reason != gossh.ResourceShortage {
		t.Fatalf("session %d: err = %v, want resource shortage", config.SSHMaxSessions+1, err)
	}
}

// TestSSHServerClosesStalledHandshake checks that a client which connects but
// never completes the key exchange is disconnected after the handshake
// timeout rather than held until the much longer idle timeout.
func TestSSHServerClosesStalledHandshake(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	// A close within waitClosed's wait can only come from the handshake
	// timeout while the idle timeout is longer.
	if idle := time.Duration(cfg.SSH.IdleTimeout) * time.Second; idle <= sessionLoopsWait {
		t.Fatalf("idle timeout %s is within the %s wait", idle, sessionLoopsWait)
	}
	addr, _ := startUnixServer(t, cfg, withHandshakeTimeout(time.Second))

	// Send a version banner and then stall in the key exchange.
	waitClosed(t, dialUnix(t, addr, "SSH-2.0-stall\r\n"), "a stalled handshake")
}
