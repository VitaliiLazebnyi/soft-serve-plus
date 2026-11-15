package daemon

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

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
func perAddrDaemon(t *testing.T, limit, perAddr int) (*GitDaemon, func(addr netip.Addr) net.Conn) {
	t.Helper()
	l := &fakeAddrListener{next: make(chan netip.Addr, 1)}
	d, addr := startDaemonOn(t, limit, func(inner net.Listener) net.Listener {
		l.Listener = inner
		return l
	}, func(d *GitDaemon) { d.cfg.Git.MaxConnectionsPerIP = perAddr })
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
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		return c
	}
	return d, from
}

// wantRefused fails the test unless the daemon turns c away for too many
// connections.
func wantRefused(t *testing.T, what string, c net.Conn) {
	t.Helper()
	_, err := readPktline(c)
	wantErrLine(t, what, err, git.ErrMaxConnections)
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
	waitConns(t, d, 1, 30*time.Second)
	from(a)
	waitConns(t, d, 2, 30*time.Second)
	wantRefused(t, "third connection from one address", from(a))
	if got := d.conns.Size(); got != perAddr {
		t.Fatalf("%d connections counted, want %d", got, perAddr)
	}

	// Another address is served while the first is at its share.
	from(b)
	waitConns(t, d, 3, 30*time.Second)

	// An IPv6 /64 is one address.
	from(netip.MustParseAddr("2001:db8::1"))
	from(netip.MustParseAddr("2001:db8::2"))
	waitConns(t, d, 5, 30*time.Second)
	wantRefused(t, "third connection from one /64", from(netip.MustParseAddr("2001:db8::3")))
	from(netip.MustParseAddr("2001:db8:0:1::1"))
	waitConns(t, d, 6, 30*time.Second)

	// Closing a connection gives its address's slot back.
	_ = first.Close()
	waitConns(t, d, 5, 30*time.Second)
	key, _ := clientPrefix(net.TCPAddrFromAddrPort(netip.AddrPortFrom(a, 1)))
	if got := d.conns.SizeOf(key); got != 1 {
		t.Fatalf("%d connections counted for %s, want 1", got, a)
	}
	from(a)
	waitConns(t, d, 6, 30*time.Second)
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
		waitConns(t, d, i+1, 30*time.Second)
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
		waitConns(t, d, i+1, 30*time.Second)
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
	if err := m.Add(c3, 10, 2); !errors.Is(err, errMaxConnectionsPerIP) || !errors.Is(err, git.ErrMaxConnections) {
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
