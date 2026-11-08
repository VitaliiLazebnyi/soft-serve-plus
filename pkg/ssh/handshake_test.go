package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
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
	addr, _ := startTestServer(t, cfg)

	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	// Send a version banner and then stall in the key exchange.
	if _, err := io.WriteString(c, "SSH-2.0-stall\r\n"); err != nil {
		t.Fatal(err)
	}

	// The idle timeout is ten minutes, so a close within a few seconds can
	// only come from the handshake timeout.
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = io.Copy(io.Discard, c)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server kept a stalled handshake open for %v", time.Since(start))
	}
}
