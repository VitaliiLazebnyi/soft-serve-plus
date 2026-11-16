package ssrf

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// errStubRefused is what a stubbed connection attempt fails with.
var errStubRefused = errors.New("stub: connection refused")

// dialRecorder records every address the secure client gets as far as
// connecting to, that is every address its checks let through, with the
// deadline of the attempt. It refuses every attempt except to the addresses
// in connect, which are dialed for real and must be local listeners, so a
// test never leaves the machine.
type dialRecorder struct {
	mu        sync.Mutex
	connect   map[string]bool
	failFirst int // refuse this many attempts before honouring connect
	dialed    []string
	deadlines []time.Time
}

func (r *dialRecorder) addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

// attempt records one attempt and reports whether it may connect.
func (r *dialRecorder) attempt(ctx context.Context, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialed = append(r.dialed, addr)
	d, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, d)
	if r.failFirst > 0 {
		r.failFirst--
		return errStubRefused
	}
	if !r.connect[addr] {
		return errStubRefused
	}
	return nil
}

// stubConnect replaces the secure client's connection attempts for the rest
// of the test; see dialRecorder. It swaps package state, so tests using it
// must not run in parallel.
func stubConnect(t *testing.T, connect ...string) *dialRecorder {
	t.Helper()
	r := &dialRecorder{connect: map[string]bool{}}
	for _, a := range connect {
		r.connect[a] = true
	}
	orig := dialContext
	dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := r.attempt(ctx, addr); err != nil {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	t.Cleanup(func() { dialContext = orig })
	return r
}
