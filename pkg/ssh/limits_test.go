package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/netip"
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
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "test",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint: gosec
		Timeout:         10 * time.Second,
	})
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
	deadline := time.Now().Add(10 * time.Second)
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
	deadline := time.Now().Add(10 * time.Second)
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

// TestSSHServerHandshakeTimeoutFreesPlace checks that a client which opens a
// TCP connection and never finishes the handshake loses its place under the
// connection limits once handshake_timeout passes, so it cannot lock others
// out by holding idle sockets.
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
			addr, _, srv := startServer(t, cfg)

			raw, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close() //nolint: errcheck
			if tc.send != "" {
				if _, err := raw.Write([]byte(tc.send)); err != nil {
					t.Fatal(err)
				}
			}

			// Wait for the server to accept the raw connection, so it holds
			// the only place before the client below dials.
			waitFor(t, "the raw connection to hold the only place", func() bool {
				srv.limiter.mu.Lock()
				defer srv.limiter.mu.Unlock()
				return srv.limiter.total == 1
			})
			if c, err := dialClient(t, addr); err == nil {
				_ = c.Close()
				t.Fatal("connection accepted while an unauthenticated one holds the only place")
			}

			// The server closes the raw connection once the handshake
			// timeout passes.
			_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 512)
			for {
				if _, err := raw.Read(buf); err != nil {
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						t.Fatal("server kept the unauthenticated connection open past the handshake timeout")
					}
					break
				}
			}

			deadline := time.Now().Add(10 * time.Second)
			for {
				c, err := dialClient(t, addr)
				if err == nil {
					_ = c.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("connection still refused after the handshake timeout: %v", err)
				}
				time.Sleep(50 * time.Millisecond)
			}
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
	addr, _ := startTestServer(t, cfg)

	c, err := dialClient(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint: errcheck
	time.Sleep(2 * time.Second)
	s, err := c.NewSession()
	if err != nil {
		t.Fatalf("authenticated connection closed by the handshake timeout: %v", err)
	}
	_ = s.Close()
}

// waitFor polls cond until it holds, failing the test after ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startSessionLimitServer starts a bare SSH server whose session channels are
// limited by limitSessions to maxSessions, running handler for each shell or
// exec request and sub for the "test" subsystem.
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
		Handler:           handler,
		SubsystemHandlers: map[string]ssh.SubsystemHandler{"test": sub},
		PublicKeyHandler:  func(ssh.Context, ssh.PublicKey) bool { return true },
		ConnCallback:      connCallback(newConnLimiter(0, 0), log.Default()),
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": limitSessions(ssh.DefaultSessionHandler, maxSessions),
		},
	}
	srv.AddHostKey(signer)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l) //nolint: errcheck
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String()
}

// TestLimitSessionsHoldsPlaceWhileHandlerRuns checks that a session keeps its
// place until the command it started has returned, not just until the client
// closes the channel, so a client cannot run more commands on one connection
// than max_sessions by closing each channel as soon as it has started it.
func TestLimitSessionsHoldsPlaceWhileHandlerRuns(t *testing.T) {
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

			// The handler still runs, so the only place stays taken however
			// long the client waits after closing its channel.
			for i := 0; i < 10; i++ {
				s2, err := c.NewSession()
				if err == nil {
					_ = s2.Close()
					t.Fatal("session accepted while a closed session's handler still runs")
				}
				if oce, ok := err.(*gossh.OpenChannelError); !ok || oce.Reason != gossh.ResourceShortage {
					t.Fatalf("err = %v, want resource shortage", err)
				}
				time.Sleep(20 * time.Millisecond)
			}

			release()
			waitFor(t, "the place to be freed after the handler returned", func() bool {
				s2, err := c.NewSession()
				if err != nil {
					return false
				}
				_ = s2.Close()
				return true
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
