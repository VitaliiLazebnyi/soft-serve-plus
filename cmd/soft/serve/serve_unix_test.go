//go:build unix

package serve

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

	ctx := context.Background()
	load, _ := testLoad(cfg)

	command := NewCommand(load)
	command.SetArgs([]string{})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	serveErr := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		serveErr <- command.ExecuteContext(ctx)
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

// signalProbe is a channel registered for SIGHUP, SIGTERM and SIGUSR1 next
// to the ones under test. It keeps those signals from killing the test
// binary, and it tells when the runtime has handed a signal to the channels
// under test.
//
// os/signal hands each signal to the registered channels in a loop over a
// map, so the probe can receive a signal before the loop reaches another
// channel: the probe having a signal does not mean the others have it yet.
// But one goroutine hands out the signals one after another, and finishes
// the loop for one before it starts the next. So once the probe has a later
// signal, every channel has been offered the earlier ones; fence sends such
// a signal. It is a SIGUSR1, which soft serve does not handle: the channels
// under test do not get it, so the fence is not itself still on its way to
// them when it returns.
type signalProbe struct {
	t *testing.T
	c chan os.Signal
}

func newSignalProbe(t *testing.T) *signalProbe {
	t.Helper()
	p := &signalProbe{t: t, c: make(chan os.Signal, 1)}
	signal.Notify(p.c, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(p.c) })
	return p
}

// send sends sig to this process and waits until the probe receives it. The
// next signal sent is then a new one, not merged into sig while it is
// pending.
func (p *signalProbe) send(sig syscall.Signal) {
	p.t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		p.t.Fatal(err)
	}
	select {
	case got := <-p.c:
		if got != sig {
			p.t.Fatalf("the probe got %v, want %v", got, sig)
		}
	case <-time.After(time.Minute):
		p.t.Fatalf("%v did not arrive within a minute", sig)
	}
}

// fence returns once every channel registered for the signals sent before it
// has been offered them.
func (p *signalProbe) fence() {
	p.t.Helper()
	p.send(syscall.SIGUSR1)
}

// stop ends the delivery to the channels notifySignals registered, so a
// soft serve run again in the same process, as tests do, leaves no handler
// behind (tracker 312).
func TestSignalsStop(t *testing.T) {
	probe := newSignalProbe(t)
	sigs := notifySignals()
	t.Cleanup(sigs.stop) // the test stops them itself too; a second stop does nothing
	probe.send(syscall.SIGHUP)
	probe.send(syscall.SIGTERM)
	probe.fence()
	for name, c := range map[string]chan os.Signal{"hup": sigs.hup, "term": sigs.term} {
		select {
		case <-c:
		default:
			t.Fatalf("the %s channel got no signal while registered", name)
		}
	}

	sigs.stop()
	probe.send(syscall.SIGHUP)
	probe.send(syscall.SIGTERM)
	probe.fence()
	select {
	case sig := <-sigs.hup:
		t.Errorf("the hup channel got %v after stop", sig)
	case sig := <-sigs.term:
		t.Errorf("the term channel got %v after stop", sig)
	default:
	}
}

// A stop signal is not lost behind SIGHUPs nobody has read yet: with one
// channel of one slot for all three signals, signal.Notify dropped it
// (CFG-21).
func TestSignalsKeepStopBehindSIGHUP(t *testing.T) {
	probe := newSignalProbe(t)
	sigs := notifySignals()
	t.Cleanup(sigs.stop)
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGTERM} {
		probe.send(sig)
	}
	probe.fence()
	// The stop and the second one, which aborts the shutdown's wait, both
	// fit before either is read.
	if n := len(sigs.term); n != 2 {
		t.Fatalf("the stop channel holds %d signals, want both SIGTERMs", n)
	}
	for range 2 {
		if sig := <-sigs.term; sig != syscall.SIGTERM {
			t.Fatalf("the stop channel got %v, want SIGTERM", sig)
		}
	}
}

// "soft serve" stops cleanly on SIGTERM while its servers serve, with
// nothing else between the start and the signal. Under -race this covers
// the shutdown racing with the goroutine that runs Start: the drain context
// must not be written to a variable that goroutine reads.
func TestServeStopsOnSIGTERM(t *testing.T) {
	probe := newSignalProbe(t)
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.SSH.ListenAddr = loopbackAddr()
	cfg.Git.ListenAddr = loopbackAddr()
	cfg.HTTP.ListenAddr = loopbackAddr()
	cfg.Stats.ListenAddr = loopbackAddr()
	load, closed := testLoad(cfg)

	command := NewCommand(load)
	command.SetArgs([]string{})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	serveErr := make(chan error, 1)
	go func() { serveErr <- command.ExecuteContext(context.Background()) }()

	// "soft serve" registers its signal handler before it starts the
	// servers, so once the HTTP server answers, a SIGTERM reaches it.
	client := &http.Client{Timeout: time.Minute}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+cfg.HTTP.ListenAddr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := client.Do(req); err == nil {
			_ = res.Body.Close()
			break
		}
		select {
		case err := <-serveErr:
			t.Fatalf("soft serve returned %v before it served", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the HTTP server did not answer within a minute")
		}
	}

	probe.send(syscall.SIGTERM)
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("soft serve returned %v after SIGTERM, want nil", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("soft serve did not stop within a minute of SIGTERM")
	}
	if !closed() {
		t.Error("soft serve left its database and log open")
	}
}
