package ssh

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/config"
	gossh "golang.org/x/crypto/ssh"
)

// prefixConn is a net.Conn whose first reads return prefix, read off the
// connection before it was handed over.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// stopServer is how a test stops an SSH server.
type stopServer func(*SSHServer) error

// A connection still in its handshake when the server stops is cut off too.
// The SSH library closes on Close, and waits for on Shutdown, only the
// connections that have finished their handshake, so one that finished it
// just after either ran was authenticated after the stop and stayed open
// until its idle timeout: the drain cut-off did not cut it off (PR #442 CI).
// The test stops the server while a client is mid-handshake, and only then
// lets the client finish it.
func TestStopCutsOffHandshakeInProgress(t *testing.T) {
	for name, stop := range map[string]stopServer{
		"close": func(s *SSHServer) error { return s.Close() },
		"shutdown": func(s *SSHServer) error {
			// Nothing has finished its handshake, so there is nothing
			// to drain.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return s.Shutdown(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			ctx := serverContext(t, cfg, log.Default())
			s, err := NewSSHServer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go s.Serve(l) //nolint: errcheck

			raw, err := (&net.Dialer{Timeout: time.Minute}).DialContext(ctx, "tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if err := raw.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			// The server's version line: it has accepted the connection
			// and is in its handshake.
			br := bufio.NewReader(raw)
			version, err := br.ReadString('\n')
			if err != nil || !strings.HasPrefix(version, "SSH-2.0-") {
				t.Fatalf("version line %q, %v", version, err)
			}

			if err := stop(s); err != nil {
				t.Fatalf("stop: %v", err)
			}

			// Finish the handshake. The server may refuse it, or let it
			// finish and then close the connection; either way the
			// connection must end.
			conn := &prefixConn{Conn: raw, r: io.MultiReader(strings.NewReader(version), br)}
			requireCutOff(t, conn, l.Addr().String(), "a connection that finished its handshake after the server stopped")
		})
	}
}

// requireCutOff finishes the client side of the SSH handshake on conn, which
// has a deadline a minute away, and fails the test unless the server cuts
// the connection off: during the handshake, or once it is done, within a
// minute. A handshake that only ran into the deadline was not cut off.
func requireCutOff(t *testing.T, conn net.Conn, addr, what string) {
	t.Helper()
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, clientConfig(t))
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: the handshake hung until its deadline instead of being cut off: %v", what, err)
		}
		return // cut off during the handshake
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close() //nolint: errcheck
	closed := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Minute):
		t.Fatalf("%s is still open a minute later", what)
	}
}

// A connection the listener accepted before the server stopped, whose
// connection callback runs only after the stop, is refused: Accept returns,
// the stop runs, and only then does the library's HandleConn reach the
// callback. Nothing else would close it, since the stop has already swept
// the connections it holds. A gate in the callback holds the connection
// there while the server stops.
func TestStopRefusesConnectionCalledBackAfter(t *testing.T) {
	for name, stop := range map[string]stopServer{
		"close": func(s *SSHServer) error { return s.Close() },
		"shutdown": func(s *SSHServer) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return s.Shutdown(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			ctx := serverContext(t, cfg, log.Default())
			s, err := NewSSHServer(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			entered, gate := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(gate) }) })
			s.srv.ConnCallback = s.live.track(func(_ ssh.Context, c net.Conn) net.Conn {
				close(entered)
				<-gate
				return c
			})
			l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go s.Serve(l) //nolint: errcheck
			raw, err := (&net.Dialer{Timeout: time.Minute}).DialContext(ctx, "tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if err := raw.SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Minute):
				t.Fatal("the connection callback was not reached within a minute")
			}

			if err := stop(s); err != nil {
				t.Fatalf("stop: %v", err)
			}
			once.Do(func() { close(gate) })

			requireCutOff(t, raw, l.Addr().String(), "a connection accepted before the stop and called back after it")
		})
	}
}
