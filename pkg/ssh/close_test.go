package ssh

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
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
			cc := clientConfig(t)
			cc.Timeout = time.Minute
			c, chans, reqs, err := gossh.NewClientConn(conn, l.Addr().String(), cc)
			if err != nil {
				return // cut off during the handshake
			}
			client := gossh.NewClient(c, chans, reqs)
			defer client.Close() //nolint: errcheck
			closed := make(chan struct{})
			go func() {
				_ = client.Wait()
				close(closed)
			}()
			if err := raw.SetDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-closed:
			case <-time.After(time.Minute):
				t.Fatal("a connection that finished its handshake after the server stopped is still open a minute later")
			}
		})
	}
}
