package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
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

// deadlineConn records the last deadline set on it.
type deadlineConn struct {
	net.Conn
	deadline time.Time
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

// TestServerConnDeadlineSubSecond checks that a timeout deadline earlier than
// the max deadline wins even when both fall in the same second.
func TestServerConnDeadlineSubSecond(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		conn func(time.Time) *serverConn
	}{
		{"init", func(maxDeadline time.Time) *serverConn {
			return &serverConn{initTimeout: time.Nanosecond, maxDeadline: maxDeadline}
		}},
		{"idle", func(maxDeadline time.Time) *serverConn {
			return &serverConn{idleTimeout: time.Nanosecond, maxDeadline: maxDeadline}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The last instant of the current second: a deadline a
			// nanosecond from now is earlier but has the same Unix second.
			maxDeadline := time.Now().Truncate(time.Second).Add(time.Second - time.Nanosecond)
			dc := &deadlineConn{}
			c := tc.conn(maxDeadline)
			c.Conn = dc
			c.updateDeadline()
			if !dc.deadline.Before(maxDeadline) {
				t.Fatalf("deadline = %v, want before max deadline %v", dc.deadline, maxDeadline)
			}
		})
	}
}
