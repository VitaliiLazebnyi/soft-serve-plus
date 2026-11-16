// Package testdial lets this module's tests keep pkg/ssrf off the network:
// the connections its HTTP clients open and the hostnames it looks up.
// Nothing outside tests sets it.
//
// It is a separate package so tests of the packages that go through pkg/ssrf,
// such as pkg/backend, can reach it; being internal, it is not part of the
// module's API.
package testdial

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
)

// Func opens a connection to address, the IP and port a secure client's
// checks have just let through, in place of a real connection to it.
type Func func(ctx context.Context, network, address string) (net.Conn, error)

// The hooks in use; nil means the real network. They are atomic because a
// dial's losing attempt can still read them after a test is done.
var (
	dial     atomic.Pointer[Func]
	resolver atomic.Pointer[net.Resolver]
)

// Set makes every connection a secure client opens go to fn, once the
// client's checks (the private address ranges and the allowlist) have let its
// address through; a nil fn restores real connections. It returns a function
// that restores the previous one.
func Set(fn Func) (restore func()) {
	var p *Func
	if fn != nil {
		p = &fn
	}
	prev := dial.Swap(p)
	return func() { dial.Store(prev) }
}

// Load returns the Func set with Set, or nil.
func Load() Func {
	if p := dial.Load(); p != nil {
		return *p
	}
	return nil
}

// SetResolver makes pkg/ssrf look up hostnames with r, wherever it would use
// net.DefaultResolver; a nil r restores that. It returns a function that
// restores the previous one.
func SetResolver(r *net.Resolver) (restore func()) {
	prev := resolver.Swap(r)
	return func() { resolver.Store(prev) }
}

// LoadResolver returns the resolver set with SetResolver, or nil.
func LoadResolver() *net.Resolver {
	return resolver.Load()
}

// errNoDNS is what NoDNS fails a lookup with.
var errNoDNS = errors.New("testdial: no DNS in tests")

// NoDNS returns a resolver that answers from the hosts file only, so
// localhost still resolves, and fails every other lookup at once without
// sending a query.
func NoDNS() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errNoDNS
		},
	}
}
