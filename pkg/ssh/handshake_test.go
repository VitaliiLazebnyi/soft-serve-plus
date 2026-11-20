package ssh

import (
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// NewSSHServer bounds the handshake with config.SSHHandshakeTimeout and
// the sessions per connection with config.SSHMaxSessions.
func TestSSHServerFixedLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	s, err := NewSSHServer(serverContext(t, cfg, log.Default()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.srv.HandshakeTimeout != config.SSHHandshakeTimeout || config.SSHHandshakeTimeout <= 0 {
		t.Errorf("handshake timeout = %s, want config.SSHHandshakeTimeout (%s)", s.srv.HandshakeTimeout, config.SSHHandshakeTimeout)
	}
	if config.SSHMaxSessions <= 0 {
		t.Errorf("config.SSHMaxSessions = %d, want a limit", config.SSHMaxSessions)
	}
}

// TestSSHServerClosesStalledHandshake checks that a client which connects but
// never completes the key exchange is disconnected after the handshake
// timeout rather than held until the much longer idle timeout.
func TestSSHServerClosesStalledHandshake(t *testing.T) {
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
