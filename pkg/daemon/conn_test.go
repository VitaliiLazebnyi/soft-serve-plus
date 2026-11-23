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
