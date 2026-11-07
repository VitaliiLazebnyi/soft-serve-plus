package daemon

import (
	"net"
	"testing"
	"time"
)

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
