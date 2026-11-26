package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/gitserve"
)

// failConn is a net.Conn whose reads and writes fail with err.
type failConn struct {
	net.Conn
	err error
}

func (c failConn) Read([]byte) (int, error)  { return 0, c.err }
func (c failConn) Write([]byte) (int, error) { return 0, c.err }

func (failConn) SetDeadline(time.Time) error { return nil }

// A network error ends the connection's context even when it comes wrapped,
// and any other error leaves it alone.
func TestServerConnCancelsOnWrappedNetError(t *testing.T) {
	t.Parallel()
	netErr := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"net error", netErr, true},
		{"wrapped net error", fmt.Errorf("conn: %w", netErr), true},
		{"other error", errors.New("boom"), false},
	} {
		for _, op := range []string{"read", "write"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				t.Parallel()
				cancelled := false
				c := &serverConn{
					Conn:          failConn{err: tc.err},
					closeCanceler: func() { cancelled = true },
				}
				if op == "read" {
					c.Read(make([]byte, 1)) //nolint:errcheck
				} else {
					c.Write([]byte("x")) //nolint:errcheck
				}
				if cancelled != tc.cancel {
					t.Errorf("%s with %v cancelled = %t, want %t", op, tc.err, cancelled, tc.cancel)
				}
			})
		}
	}
}

// TestServerConnDeadlineSubSecond checks that a timeout deadline earlier than
// the max deadline wins even when both fall in the same second.
//
// The deadlines are worked out at a fixed time rather than the time the test
// runs, so a stall between setting them up and comparing them cannot carry
// the timeout deadline past the max one.
func TestServerConnDeadlineSubSecond(t *testing.T) {
	t.Parallel()
	// A whole second: a deadline a nanosecond from now is earlier than
	// the last instant of this second but has the same Unix second.
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	maxDeadline := now.Add(time.Second - time.Nanosecond)
	for _, tc := range []struct {
		name string
		read bool
		conn *serverConn
	}{
		{"init", true, &serverConn{initDeadline: now.Add(time.Nanosecond), maxDeadline: maxDeadline}},
		{"idle", false, &serverConn{idleTimeout: time.Nanosecond, maxDeadline: maxDeadline}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, want := tc.conn.deadlineAt(tc.read, now), now.Add(time.Nanosecond); !got.Equal(want) {
				t.Fatalf("deadline = %v, want %v, before max deadline %v", got, want, maxDeadline)
			}
		})
	}
}

// moveConn records the deadlines set on it, and takes every write and
// answers every read at once.
type moveConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *moveConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}

func (c *moveConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *moveConn) Read(p []byte) (int, error)  { return len(p), nil }

// TestServerConnMovesIdleDeadline checks that every read and every write
// after the request moves the connection's deadline to the idle timeout
// from then, so that upload-pack's keepalives keep a quiet fetch from
// reaching the idle timeout, which TestQuietCloneSlowPack, whose connection
// keeps no deadlines, cannot show.
//
// Each deadline is compared with the time taken before the call, so a
// stall only moves it later.
func TestServerConnMovesIdleDeadline(t *testing.T) {
	t.Parallel()
	const idle = time.Hour
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			mc := &moveConn{}
			c := &serverConn{Conn: mc, idleTimeout: idle}
			for i := 1; i <= 3; i++ {
				before := time.Now()
				var err error
				if op == "read" {
					_, err = c.Read(make([]byte, 5))
				} else {
					_, err = c.Write([]byte("0005\x01"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(mc.deadlines) != i {
					t.Fatalf("after %d %ss, %d deadlines set, want %d", i, op, len(mc.deadlines), i)
				}
				if got, want := mc.deadlines[i-1], before.Add(idle); got.Before(want) {
					t.Fatalf("%s %d: deadline %v, want the idle timeout after the %s, from %v", op, i, got, op, want)
				}
			}
		})
	}
}

// fakeAddrListener gives each accepted connection the remote address the
// test queued for it, if any, so that connections over loopback can stand
// for clients at other addresses.
type fakeAddrListener struct {
	net.Listener
	next chan netip.Addr
}

func (l *fakeAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case a := <-l.next:
		return &fakeAddrConn{Conn: c, remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(a, 40000))}, nil
	default:
		return c, nil
	}
}

// fakeAddrConn is a connection with another remote address.
type fakeAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *fakeAddrConn) RemoteAddr() net.Addr { return c.remote }

// NetConn returns the wrapped connection, so the daemon can still shut its
// write side when it refuses the client.
func (c *fakeAddrConn) NetConn() net.Conn { return c.Conn }

// perAddrDaemon serves a daemon allowing limit connections in all and
// perAddr from one address. Its from function dials it as a client at addr,
// or at loopback if addr is the zero Addr.
func perAddrDaemon(t *testing.T, limit, perAddr int) (*Server, func(addr netip.Addr) net.Conn) {
	t.Helper()
	l := &fakeAddrListener{next: make(chan netip.Addr, 1)}
	d, addr := startDaemonOn(t, limit, func(inner net.Listener) net.Listener {
		l.Listener = inner
		return l
	}, func(d *Server) { d.cfg.Git.MaxConnectionsPerIP = perAddr })
	from := func(a netip.Addr) net.Conn {
		t.Helper()
		if a.IsValid() {
			l.next <- a
		}
		c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetReadDeadline(time.Now().Add(idleWait))
		return c
	}
	return d, from
}

// wantRefused fails the test unless the daemon turns c away for too many
// connections.
func wantRefused(t *testing.T, what string, c net.Conn) {
	t.Helper()
	_, err := readPktline(c)
	wantErrLine(t, what, err, gitserve.ErrMaxConnections)
}

// TestMaxConnectionsPerIP checks that one address cannot take every slot:
// past its share its connections are refused while another address is still
// served, an IPv6 /64 counts as one address, and a closed connection gives
// its address's slot back.
func TestMaxConnectionsPerIP(t *testing.T) {
	t.Parallel()
	const limit, perAddr = 6, 2
	d, from := perAddrDaemon(t, limit, perAddr)
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")

	first := from(a)
	waitConns(t, d, 1, idleWait)
	from(a)
	waitConns(t, d, 2, idleWait)
	wantRefused(t, "third connection from one address", from(a))
	if got := d.conns.Size(); got != perAddr {
		t.Fatalf("%d connections counted, want %d", got, perAddr)
	}

	// Another address is served while the first is at its share.
	from(b)
	waitConns(t, d, 3, idleWait)

	// An IPv6 /64 is one address.
	from(netip.MustParseAddr("2001:db8::1"))
	from(netip.MustParseAddr("2001:db8::2"))
	waitConns(t, d, 5, idleWait)
	wantRefused(t, "third connection from one /64", from(netip.MustParseAddr("2001:db8::3")))
	from(netip.MustParseAddr("2001:db8:0:1::1"))
	waitConns(t, d, 6, idleWait)

	// Closing a connection gives its address's slot back.
	_ = first.Close()
	waitConns(t, d, 5, idleWait)
	key, _ := clientPrefix(net.TCPAddrFromAddrPort(netip.AddrPortFrom(a, 1)))
	if got := d.conns.SizeOf(key); got != 1 {
		t.Fatalf("%d connections counted for %s, want 1", got, a)
	}
	from(a)
	waitConns(t, d, 6, idleWait)
}

// TestMaxConnectionsPerIPLoopbackExempt checks that loopback clients are
// bound only by the total, so that a TCP proxy on the same host is not held
// to one address's share.
func TestMaxConnectionsPerIPLoopbackExempt(t *testing.T) {
	t.Parallel()
	const limit = 5
	d, from := perAddrDaemon(t, limit, 1)
	var loopback netip.Addr // the zero Addr dials from the real loopback address
	for i, a := range []netip.Addr{
		loopback, loopback, loopback,
		netip.MustParseAddr("::1"),
		netip.MustParseAddr("::ffff:127.0.0.2"),
	} {
		from(a)
		waitConns(t, d, i+1, idleWait)
	}
	wantRefused(t, "connection over the total", from(netip.Addr{}))
}

// TestMaxConnectionsPerIPZero checks that 0 sets no per-address limit.
func TestMaxConnectionsPerIPZero(t *testing.T) {
	t.Parallel()
	d, from := perAddrDaemon(t, 3, 0)
	a := netip.MustParseAddr("192.0.2.1")
	for i := range 3 {
		from(a)
		waitConns(t, d, i+1, idleWait)
	}
	wantRefused(t, "connection over the total", from(a))
}

func TestConnectionsPerAddr(t *testing.T) {
	t.Parallel()
	m := newConnections()
	conn := func(addr string) net.Conn {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		return &fakeAddrConn{Conn: a, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr))}
	}
	c1, c2, c3 := conn("192.0.2.1:1"), conn("192.0.2.1:2"), conn("[::ffff:192.0.2.1]:3")
	if err := m.Add(c1, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(c2, 10, 2); err != nil {
		t.Fatal(err)
	}
	// The IPv4-mapped form is the same address.
	if err := m.Add(c3, 10, 2); !errors.Is(err, errMaxConnectionsPerIP) || !errors.Is(err, gitserve.ErrMaxConnections) {
		t.Fatalf("Add over the share = %v, want errMaxConnectionsPerIP", err)
	}
	// A pipe has no IP address and counts only towards the total.
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if err := m.Add(a, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(c1); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(c3, 10, 2); err != nil {
		t.Fatalf("Add after a Close = %v", err)
	}
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if got := len(m.perAddr); got != 0 {
		t.Fatalf("%d addresses counted after CloseAll", got)
	}
}
