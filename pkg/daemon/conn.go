package daemon

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/git"
)

// connections is a synchronizes access to to a net.Conn pool.
type connections struct {
	m      map[net.Conn]struct{}
	mu     sync.Mutex
	closed bool // set by CloseAll; no connection is added after it
}

// Add adds c to the pool unless the pool already holds limit connections,
// which returns git.ErrMaxConnections, or CloseAll has run, which returns
// ErrServerClosed. Checking and adding under one lock keeps a burst of
// connections from all passing the check before any of them is counted.
func (m *connections) Add(c net.Conn, limit int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrServerClosed
	}
	if len(m.m) >= limit {
		return git.ErrMaxConnections
	}
	m.m[c] = struct{}{}
	return nil
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
	m.closed = true
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

	initTimeout   time.Duration
	idleTimeout   time.Duration
	maxDeadline   time.Time
	closeCanceler context.CancelFunc
}

var _ net.Conn = (*serverConn)(nil)

func (c *serverConn) Write(p []byte) (n int, err error) {
	c.updateDeadline()
	n, err = c.Conn.Write(p)
	if _, isNetErr := err.(net.Error); isNetErr && c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

func (c *serverConn) Read(b []byte) (n int, err error) {
	c.updateDeadline()
	n, err = c.Conn.Read(b)
	if _, isNetErr := err.(net.Error); isNetErr && c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

func (c *serverConn) Close() (err error) {
	err = c.Conn.Close()
	if c.closeCanceler != nil {
		c.closeCanceler()
	}
	return
}

func (c *serverConn) updateDeadline() {
	switch {
	case c.initTimeout > 0:
		initTimeout := time.Now().Add(c.initTimeout)
		c.initTimeout = 0
		if initTimeout.Unix() < c.maxDeadline.Unix() || c.maxDeadline.IsZero() {
			c.Conn.SetDeadline(initTimeout) //nolint: errcheck
			return
		}
	case c.idleTimeout > 0:
		idleDeadline := time.Now().Add(c.idleTimeout)
		if idleDeadline.Unix() < c.maxDeadline.Unix() || c.maxDeadline.IsZero() {
			c.Conn.SetDeadline(idleDeadline) //nolint: errcheck
			return
		}
	}
	c.Conn.SetDeadline(c.maxDeadline) //nolint: errcheck
}
