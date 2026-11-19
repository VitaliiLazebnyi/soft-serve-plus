package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/config"
	gossh "golang.org/x/crypto/ssh"
)

func tcpAddr(s string) net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s))
}

// acquire calls l.acquire and drops the reason.
func acquire(l *connLimiter, addr net.Addr) func() {
	release, _ := l.acquire(addr)
	return release
}

func TestConnLimiterReason(t *testing.T) {
	l := newConnLimiter(2, 1)
	if r, _ := l.acquire(tcpAddr("192.0.2.1:1")); r == nil {
		t.Fatal("first connection refused")
	}
	if r, why := l.acquire(tcpAddr("192.0.2.1:2")); r != nil || why != refusedPerIP {
		t.Fatalf("second connection from one address: reason %q, want %q", why, refusedPerIP)
	}
	if r, _ := l.acquire(tcpAddr("192.0.2.2:1")); r == nil {
		t.Fatal("connection from another address refused")
	}
	if r, why := l.acquire(tcpAddr("192.0.2.3:1")); r != nil || why != refusedTotal {
		t.Fatalf("third connection: reason %q, want %q", why, refusedTotal)
	}
}

func TestConnLimiter(t *testing.T) {
	l := newConnLimiter(3, 2)
	a1 := acquire(l, tcpAddr("192.0.2.1:1000"))
	a2 := acquire(l, tcpAddr("192.0.2.1:1001"))
	if a1 == nil || a2 == nil {
		t.Fatal("first two connections from one address refused")
	}
	if acquire(l, tcpAddr("192.0.2.1:1002")) != nil {
		t.Fatal("third connection from one address accepted, want refused by per-ip limit")
	}
	b := acquire(l, tcpAddr("192.0.2.2:1000"))
	if b == nil {
		t.Fatal("connection from another address refused")
	}
	if acquire(l, tcpAddr("192.0.2.3:1000")) != nil {
		t.Fatal("fourth connection accepted, want refused by total limit")
	}
	a1()
	a1() // releasing twice must not free a second place
	if acquire(l, tcpAddr("192.0.2.1:1003")) == nil {
		t.Fatal("connection refused after one from the same address closed")
	}
	if acquire(l, tcpAddr("192.0.2.3:1000")) != nil {
		t.Fatal("double release freed an extra place")
	}
	a2()
	b()
	if l.total != 1 || len(l.perIP) != 1 {
		t.Fatalf("total = %d, perIP = %v; want 1 connection from one address", l.total, l.perIP)
	}
}

func TestConnLimiterGroupsIPv6By64(t *testing.T) {
	l := newConnLimiter(0, 1)
	if acquire(l, tcpAddr("[2001:db8:1:2::1]:22")) == nil {
		t.Fatal("first IPv6 connection refused")
	}
	if acquire(l, tcpAddr("[2001:db8:1:2::ffff]:22")) != nil {
		t.Fatal("second connection from the same /64 accepted")
	}
	if acquire(l, tcpAddr("[2001:db8:1:3::1]:22")) == nil {
		t.Fatal("connection from another /64 refused")
	}
	// An IPv4-mapped address counts as its IPv4 address.
	if acquire(l, tcpAddr("192.0.2.9:22")) == nil {
		t.Fatal("IPv4 connection refused")
	}
	if acquire(l, tcpAddr("[::ffff:192.0.2.9]:22")) != nil {
		t.Fatal("IPv4-mapped connection not counted with its IPv4 address")
	}
}

func TestConnLimiterUnlimited(t *testing.T) {
	l := newConnLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if acquire(l, tcpAddr("192.0.2.1:1000")) == nil {
			t.Fatal("connection refused with limits off")
		}
	}
}

func dialClient(t *testing.T, addr string) (*gossh.Client, error) {
	t.Helper()
	cfg := clientConfig(t)
	cfg.Timeout = sessionLoopsWait
	return gossh.Dial("tcp", addr, cfg)
}

// clientConfig returns the configuration of a test client, which signs in
// as test with a new key and accepts any host key.
func clientConfig(t *testing.T) *gossh.ClientConfig {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
	}
}

func TestSSHServerMaxConnectionsPerIP(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.MaxConnectionsPerIP = 2
	addr, _ := startTestServer(t, cfg)

	var clients []*gossh.Client
	for i := 0; i < 2; i++ {
		c, err := dialClient(t, addr)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		clients = append(clients, c)
	}
	if c, err := dialClient(t, addr); err == nil {
		_ = c.Close()
		t.Fatal("third connection from one address accepted, want refused")
	}

	_ = clients[0].Close()
	// The server releases the place once it has seen the close.
	deadline := time.Now().Add(time.Minute)
	for {
		c, err := dialClient(t, addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection still refused after another closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = clients[1].Close()
}

func TestSSHServerMaxConnections(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.MaxConnections = 1
	cfg.SSH.MaxConnectionsPerIP = 0
	addr, _ := startTestServer(t, cfg)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	if c2, err := dialClient(t, addr); err == nil {
		_ = c2.Close()
		t.Fatal("second connection accepted, want refused by max_connections")
	}
}

func TestSSHServerMaxSessions(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.MaxSessions = 2
	addr, _ := startTestServer(t, cfg)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	s1, err := c.NewSession()
	if err != nil {
		t.Fatalf("session 1: %v", err)
	}
	s2, err := c.NewSession()
	if err != nil {
		t.Fatalf("session 2: %v", err)
	}
	if s3, err := c.NewSession(); err == nil {
		_ = s3.Close()
		t.Fatal("third session on one connection accepted, want refused")
	} else if oce, ok := err.(*gossh.OpenChannelError); !ok || oce.Reason != gossh.ResourceShortage {
		t.Fatalf("third session: err = %v, want resource shortage", err)
	}

	_ = s1.Close()
	deadline := time.Now().Add(time.Minute)
	for {
		s, err := c.NewSession()
		if err == nil {
			_ = s.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still refused after another closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = s2.Close()
}

// A server built by NewSSHServer refuses a command while max_sessions
// commands run, even when their channels are closed.
//
// The place is held here by a TUI session whose channel the client then
// closes. The TUI program is bound to the connection's context, not the
// channel's (newSessionProgram passes tea.WithContext(s.Context())), so
// closing the channel does not stop it: it keeps its handler slot until the
// connection closes. That is the ControlMaster case limitHandlers exists
// for, and what makes this a test of commands counted, not channels.
func TestSSHServerMaxSessionsCountsCommands(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.MaxSessions = 1
	addr, _ := startTestServer(t, cfg)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	tui, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := tui.RequestPty("xterm", 40, 80, nil); err != nil {
		t.Fatal(err)
	}
	stdout, err := tui.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := tui.Shell(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	_ = tui.Close()

	for i := 0; i < 3; i++ {
		var s *gossh.Session
		waitFor(t, "the closed channel's place", func() bool {
			s, err = c.NewSession()
			return err == nil
		})
		done := make(chan error, 1)
		var stderr bytes.Buffer
		s.Stderr = &stderr
		go func() { done <- s.Run("repo list") }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(stderr.String(), msgTooManySessions) {
				t.Fatalf("command ran while the TUI runs: %v: %q", err, stderr.String())
			}
		case <-time.After(sessionLoopsWait):
			t.Fatal("command neither refused nor finished")
		}
		_ = s.Close()
	}
}

// TestSSHServerHandshakeTimeoutFreesPlace checks that a client which opens a
// connection and never finishes the handshake loses its place under the
// connection limits once handshake_timeout passes, so it cannot lock others
// out by holding idle sockets.
//
// The test sees the timeout fire in the server's state: the connection is
// closed and its place released. It does not check that a second client is
// refused while the first holds the place: with a one-second timeout that
// check races the timeout, and on a loaded machine the second client can
// reach the server after the first connection is closed and be let in.
// TestSSHServerMaxConnections checks the refusal. The server listens on a
// Unix socket, so no loopback TCP connect can stall either.
func TestSSHServerHandshakeTimeoutFreesPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		send string
	}{
		{"silent", ""},
		{"partial banner", "SSH-2.0-"},
		{"banner only", "SSH-2.0-test\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.DataPath = t.TempDir()
			cfg.SSH.MaxConnections = 1
			cfg.SSH.MaxConnectionsPerIP = 0
			cfg.SSH.IdleTimeout = 0 // the handshake timeout alone must free the place
			cfg.SSH.HandshakeTimeout = 1
			addr, srv := startUnixServer(t, cfg)
			places := func() int {
				srv.limiter.mu.Lock()
				defer srv.limiter.mu.Unlock()
				return srv.limiter.total
			}

			raw := dialUnix(t, addr, tc.send)
			waitFor(t, "the raw connection to take the only place", func() bool { return places() == 1 })

			// The server closes the raw connection once the handshake
			// timeout passes, and gives its place back.
			waitClosed(t, raw, "the unauthenticated connection")
			waitFor(t, "the unauthenticated connection's place to be released", func() bool { return places() == 0 })

			// A client that finishes its handshake in time gets the place.
			// One slowed past the timeout is closed and tries again.
			waitFor(t, "a client to get the released place", func() bool {
				c, err := dialUnixClient(t, addr)
				if err != nil {
					return false
				}
				_ = c.Close()
				return true
			})
		})
	}
}

// TestSSHServerHandshakeTimeoutKeepsAuthenticated checks that the handshake
// timeout stops applying once a client has authenticated.
func TestSSHServerHandshakeTimeoutKeepsAuthenticated(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.IdleTimeout = 0
	cfg.SSH.HandshakeTimeout = 1
	addr, _ := startUnixServer(t, cfg)

	// A client slowed past the timeout in its handshake is closed and
	// tries again.
	var c *gossh.Client
	waitFor(t, "a client to authenticate within the handshake timeout", func() bool {
		var err error
		c, err = dialUnixClient(t, addr)
		return err == nil
	})
	defer c.Close() //nolint: errcheck

	// A connection accepted after c's that never starts its handshake is
	// closed once its own handshake timeout passes. c's timeout, which
	// started earlier, has then passed too, whatever the machine's load.
	waitClosed(t, dialUnix(t, addr, ""), "a connection opened after the authenticated one")

	s, err := c.NewSession()
	if err != nil {
		t.Fatalf("authenticated connection closed by the handshake timeout: %v", err)
	}
	_ = s.Close()
}

// waitFor polls cond until it holds, failing the test after
// sessionLoopsWait: the conditions waited for mostly need the server to read
// a close from the client, which can stall as long on a loaded machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(sessionLoopsWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startUnixServer starts an SSH server built by NewSSHServer with cfg on a
// Unix socket, and returns the socket's path and the server. A connection to
// it has no TCP connect for a loaded machine to stall, which matters to a
// test whose clients must finish a step within a short server timeout. (An
// in-memory pipe would not do: both ends of an SSH connection send their
// version before reading, which an unbuffered pipe deadlocks.) Its
// connections carry no IP address, so they count only towards
// max_connections.
func startUnixServer(t *testing.T, cfg *config.Config) (string, *SSHServer) {
	t.Helper()
	ctx := serverContext(t, cfg, log.Default())
	s, err := NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir: a socket path must stay within about 100 bytes.
	dir, err := os.MkdirTemp("", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = s.Close() })
	return l.Addr().String(), s
}

// dialUnix opens a connection to the socket at path that sends send and
// then nothing.
func dialUnix(t *testing.T, path, send string) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, send); err != nil {
		t.Fatal(err)
	}
	return c
}

// dialUnixClient is dialClient to the socket at path. Its handshake gives up
// after sessionLoopsWait, which only a server that never answers reaches.
func dialUnixClient(t *testing.T, path string) (*gossh.Client, error) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(sessionLoopsWait))
	cc, chans, reqs, err := gossh.NewClientConn(conn, path, clientConfig(t))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return gossh.NewClient(cc, chans, reqs), nil
}

// waitClosed reads conn until the server closes it, and fails the test if it
// is still open after sessionLoopsWait.
func waitClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(sessionLoopsWait))
	if _, err := io.Copy(io.Discard, conn); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the server kept %s open for %s", what, sessionLoopsWait)
	}
}

// startSessionLimitServer starts a bare SSH server whose session handling is
// set up by setSessionHandlers with maxSessions, running handler for each
// shell or exec request, and sub for the "test" subsystem, also limited by
// limitHandlers (the server itself has no subsystems).
func startSessionLimitServer(t *testing.T, maxSessions int, handler ssh.Handler, sub ssh.SubsystemHandler) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := &ssh.Server{
		Handler: handler,
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"test": ssh.SubsystemHandler(limitHandlers(ssh.Handler(sub), maxSessions)),
		},
		PublicKeyHandler: func(ssh.Context, ssh.PublicKey) bool { return true },
		ConnCallback:     connCallback(newConnLimiter(0, 0), log.Default(), 0),
	}
	setSessionHandlers(srv, maxSessions)
	srv.AddHostKey(signer)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String()
}

// TestLimitHandlersWhileClosedSessionRuns checks that a command keeps its
// place until it has returned, not just until the client closes its channel,
// so a client cannot run more commands on one connection than max_sessions by
// closing each channel as soon as it has started it. The closed channel
// itself gives its place back.
func TestLimitHandlersWhileClosedSessionRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(*gossh.Session) error
	}{
		{"exec", func(s *gossh.Session) error { return s.Start("slow") }},
		{"shell", func(s *gossh.Session) error { return s.Shell() }},
		{"subsystem", func(s *gossh.Session) error { return s.RequestSubsystem("test") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			unblock := make(chan struct{})
			var unblockOnce sync.Once
			release := func() { unblockOnce.Do(func() { close(unblock) }) }
			t.Cleanup(release)
			block := func(ssh.Session) {
				started <- struct{}{}
				<-unblock
			}
			addr := startSessionLimitServer(t, 1, block, block)

			c, err := dialClient(t, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close() //nolint: errcheck

			s, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.start(s); err != nil {
				t.Fatal(err)
			}
			<-started
			_ = s.Close()

			// The channel's place comes back once the server has seen the
			// close, but the command still runs, so a second one is
			// refused however long the client waits.
			for i := 0; i < 3; i++ {
				var s2 *gossh.Session
				waitFor(t, "the closed channel's place", func() bool {
					s2, err = c.NewSession()
					return err == nil
				})
				stderr, err := s2.StderrPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := tc.start(s2); err != nil {
					t.Fatal(err)
				}
				// The server ends the session, which ends stderr.
				refused := make(chan []byte, 1)
				go func() {
					msg, _ := io.ReadAll(stderr)
					refused <- msg
				}()
				select {
				case msg := <-refused:
					if !strings.Contains(string(msg), msgTooManySessions) {
						t.Errorf("stderr = %q, want %q", msg, msgTooManySessions)
					}
				case <-started:
					t.Fatal("a second command started while a closed session's command still runs")
				case <-time.After(sessionLoopsWait):
					t.Fatal("the second session was neither refused nor started")
				}
				_ = s2.Close()
				time.Sleep(20 * time.Millisecond)
			}

			release()
			waitFor(t, "a command to start after the first returned", func() bool {
				s2, err := c.NewSession()
				if err != nil {
					return false
				}
				defer s2.Close() //nolint: errcheck
				if err := tc.start(s2); err != nil {
					return false
				}
				select {
				case <-started:
					return true
				case <-time.After(time.Second): // one attempt; waitFor retries for a minute
					return false
				}
			})
		})
	}
}

// TestLimitSessionsFreesPlaceWithoutHandler checks that a session which never
// starts a handler gives its place back when the client closes it, including
// one whose subsystem request was refused.
func TestLimitSessionsFreesPlaceWithoutHandler(t *testing.T) {
	addr := startSessionLimitServer(t, 1, func(ssh.Session) {}, func(ssh.Session) {})
	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck

	for i := 0; i < 3; i++ {
		var s *gossh.Session
		waitFor(t, "a session", func() bool {
			s, err = c.NewSession()
			return err == nil
		})
		if i == 1 {
			if err := s.RequestSubsystem("missing"); err == nil {
				t.Fatal("unknown subsystem accepted")
			}
		}
		_ = s.Close()
	}
}
