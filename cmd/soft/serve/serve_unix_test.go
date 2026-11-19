//go:build unix

package serve

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// servedCertCN returns the common name of the certificate the TLS server at
// addr presents, or an error if the handshake fails.
func servedCertCN(addr string) (string, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second}, // one attempt; waitServedCert retries for a minute
		Config:    &tls.Config{InsecureSkipVerify: true}, //nolint: gosec // self-signed test certificate
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // one attempt; waitServedCert retries for a minute
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint: errcheck
	tconn, ok := conn.(*tls.Conn)
	if !ok {
		return "", io.ErrUnexpectedEOF
	}
	certs := tconn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", io.ErrUnexpectedEOF
	}
	return certs[0].Subject.CommonName, nil
}

// waitServedCert polls the TLS server at addr until it presents the
// certificate named want. It fails the test if "soft serve" exits first or
// the server does not present it within a minute.
func waitServedCert(t *testing.T, addr, want string, serveErr <-chan error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var got string
	var err error
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			t.Fatalf("soft serve exited while waiting for certificate %q: %v", want, err)
		default:
		}
		got, err = servedCertCN(addr)
		if err == nil && got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server presents certificate %q (err %v), want %q", got, err, want)
}

// "soft serve" reloads the HTTP server's TLS certificate from disk on
// SIGHUP and keeps serving, and shuts down cleanly on SIGTERM.
func TestServeReloadsCertificateOnSIGHUP(t *testing.T) {
	// A SIGHUP or SIGTERM that reaches a process with no handler for it
	// kills the process. Hold a handler for the whole test so a signal sent
	// before "soft serve" registers its own fails this test instead of
	// killing the test binary. Signals go to every registered channel, so
	// this does not hide them from "soft serve".
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGHUP, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(guard) })

	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	certPath := filepath.Join(t.TempDir(), "cert.pem")
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	generateTestCert(t, certPath, keyPath, "cert-v1")
	cfg.HTTP.TLSCertPath = certPath
	cfg.HTTP.TLSKeyPath = keyPath
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.SSH.ListenAddr = loopbackAddr()
	cfg.Git.ListenAddr = loopbackAddr()
	cfg.HTTP.ListenAddr = loopbackAddr()
	cfg.Stats.ListenAddr = loopbackAddr()

	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	Command.SetArgs([]string{})
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	serveErr := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		serveErr <- Command.ExecuteContext(ctx)
	}()
	t.Cleanup(func() {
		// Stop "soft serve" if the test failed while it was running.
		select {
		case <-exited:
			return
		default:
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(time.Minute):
			t.Errorf("soft serve still running a minute after SIGTERM")
		}
	})

	// "soft serve" registers its signal handler before it starts the
	// servers, so once the HTTP server answers, a SIGHUP reaches it.
	waitServedCert(t, cfg.HTTP.ListenAddr, "cert-v1", serveErr)

	generateTestCert(t, certPath, keyPath, "cert-v2")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	waitServedCert(t, cfg.HTTP.ListenAddr, "cert-v2", serveErr)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("soft serve returned %v after SIGTERM, want nil", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("soft serve did not stop within a minute of SIGTERM")
	}
}
