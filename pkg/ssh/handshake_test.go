package ssh

import (
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func TestDefaultHandshakeTimeout(t *testing.T) {
	if got := config.DefaultConfig().SSH.HandshakeTimeout; got <= 0 {
		t.Errorf("default ssh handshake timeout = %d, want a positive number of seconds", got)
	}
}

// TestSSHServerClosesStalledHandshake checks that a client which connects but
// never completes the key exchange is disconnected after the handshake
// timeout rather than held until the much longer idle timeout.
func TestSSHServerClosesStalledHandshake(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.HandshakeTimeout = 1
	// A close within waitClosed's wait can only come from the handshake
	// timeout while the idle timeout is longer.
	if idle := time.Duration(cfg.SSH.IdleTimeout) * time.Second; idle <= sessionLoopsWait {
		t.Fatalf("idle timeout %s is within the %s wait", idle, sessionLoopsWait)
	}
	addr, _ := startUnixServer(t, cfg)

	// Send a version banner and then stall in the key exchange.
	waitClosed(t, dialUnix(t, addr, "SSH-2.0-stall\r\n"), "a stalled handshake")
}
