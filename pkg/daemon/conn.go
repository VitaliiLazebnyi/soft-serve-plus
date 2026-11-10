package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// connections is a synchronizes access to to a net.Conn pool.
type connections struct {
	m  map[net.Conn]struct{}
	mu sync.Mutex
}

func (m *connections) Add(c net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[c] = struct{}{}
}

func (m *connections) Close(c net.Conn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := c.Close()
	delete(m.m, c)
	return err
}

func (m *connections) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

func (m *connections) CloseAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	for c := range m.m {
		err = errors.Join(err, c.Close())
		delete(m.m, c)
	}

	return err
}

// serverConn is a wrapper around a net.Conn that closes the connection when
// the one of the timeouts is reached.
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
