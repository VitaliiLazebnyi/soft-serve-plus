package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/soft-serve/internal/clientaddr"
	"github.com/charmbracelet/soft-serve/pkg/git"
)

// errMaxConnectionsPerIP is returned by connections.Add for a connection
// over its address's share. It wraps git.ErrMaxConnections, which is what the
// client is told.
var errMaxConnectionsPerIP = fmt.Errorf("%w from one address", git.ErrMaxConnections)

// connections is a net.Conn pool that is safe for concurrent use. It counts
// the connections in all and from each client address.
type connections struct {
	// m maps each connection to the key it is counted under in perAddr,
	// or to the zero Prefix if it is not.
	m       map[net.Conn]netip.Prefix
	perAddr map[netip.Prefix]int
	mu      sync.Mutex
	closed  bool // set by CloseAll; no connection is added after it
}

// newConnections returns an empty pool.
func newConnections() connections {
	return connections{
		m:       make(map[net.Conn]netip.Prefix),
		perAddr: make(map[netip.Prefix]int),
	}
}

// clientPrefix returns the key a connection from addr is counted under
// against the per-address limit; see clientaddr.Prefix. ok is false when
// addr holds no IP address, or a loopback one: such a connection only counts
// towards the total. A TCP proxy on the same host connects from loopback,
// and every client it forwards would otherwise share one address's share.
func clientPrefix(addr net.Addr) (netip.Prefix, bool) {
	if addr == nil {
		return netip.Prefix{}, false
	}
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil || ap.Addr().Unmap().IsLoopback() {
		return netip.Prefix{}, false
	}
	return clientaddr.Prefix(ap.Addr())
}

// Add adds c to the pool unless the pool already holds limit connections,
// which returns git.ErrMaxConnections, or perAddr connections from c's
// address, which returns errMaxConnectionsPerIP, or CloseAll has run, which
// returns ErrServerClosed. A perAddr of 0 sets no per-address limit.
// Checking and adding under one lock keeps a burst of connections from all
// passing the check before any of them is counted.
func (m *connections) Add(c net.Conn, limit, perAddr int) error {
	key, keyed := clientPrefix(c.RemoteAddr())
	keyed = keyed && perAddr > 0
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrServerClosed
	}
	if len(m.m) >= limit {
		return git.ErrMaxConnections
	}
	if keyed && m.perAddr[key] >= perAddr {
		return errMaxConnectionsPerIP
	}
	if keyed {
		m.perAddr[key]++
	} else {
		key = netip.Prefix{}
	}
	m.m[c] = key
	return nil
}

func (m *connections) Close(c net.Conn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := c.Close()
	m.remove(c)
	return err
}

// remove takes c out of the pool and its address's count. m.mu must be held.
func (m *connections) remove(c net.Conn) {
	key, ok := m.m[c]
	if !ok {
		return
	}
	delete(m.m, c)
	if key.IsValid() {
		if m.perAddr[key]--; m.perAddr[key] <= 0 {
			delete(m.perAddr, key)
		}
	}
}

// SizeOf returns how many connections are counted under key.
func (m *connections) SizeOf(key netip.Prefix) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perAddr[key]
}

func (m *connections) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

func (m *connections) CloseAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	var err error
	for c := range m.m {
		err = errors.Join(err, c.Close())
		m.remove(c)
	}

	return err
}

// serverConn is a wrapper around a net.Conn that closes the connection when
// one of its timeouts is reached.
type serverConn struct {
	net.Conn

	// initDeadline, while set, bounds reading the request line as a whole,
	// so a client trickling it in a byte at a time cannot keep pushing the
	// idle deadline forward. It is set before the request is read and
	// cleared once it has been, never while another goroutine uses the
	// connection. Only reads honor it: a write, such as the error the
	// daemon sends once this deadline has passed, still gets the idle and
	// maximum deadlines, and Write never reads the field.
	initDeadline  time.Time
	idleTimeout   time.Duration
	maxDeadline   time.Time
	closeCanceler context.CancelFunc

	// wrote records that some output reached the client.
	wrote atomic.Bool
}

var _ net.Conn = (*serverConn)(nil)

func (c *serverConn) Write(p []byte) (n int, err error) {
	c.updateDeadline(false)
	n, err = c.Conn.Write(p)
	if n > 0 {
		c.wrote.Store(true)
	}
	if isNetError(err) && c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

func (c *serverConn) Read(b []byte) (n int, err error) {
	c.updateDeadline(true)
	n, err = c.Conn.Read(b)
	if isNetError(err) && c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

// Wrote reports whether any output was written to the client. Once it was,
// an ERR packet can no longer be sent: the client would read it as part of
// the stream, for example as a sideband packet of the pack.
func (c *serverConn) Wrote() bool {
	return c.wrote.Load()
}

func (c *serverConn) Close() (err error) {
	err = c.Conn.Close()
	if c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

// updateDeadline sets the connection deadline to the earliest of the idle
// deadline, the maximum deadline and, for a read before the request has been
// read, the request deadline.
func (c *serverConn) updateDeadline(read bool) {
	deadline := c.maxDeadline
	earlier := func(t time.Time) {
		if deadline.IsZero() || t.Before(deadline) {
			deadline = t
		}
	}
	if read && !c.initDeadline.IsZero() {
		earlier(c.initDeadline)
	}
	if c.idleTimeout > 0 {
		earlier(time.Now().Add(c.idleTimeout))
	}
	c.Conn.SetDeadline(deadline) //nolint: errcheck
}

// isNetError reports whether err is, or wraps, a net.Error.
func isNetError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}
