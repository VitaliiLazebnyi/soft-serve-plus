package ssh

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"charm.land/log/v2"
	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// connLimiter caps the number of SSH connections open at once, in total and
// per client address. A zero limit turns that check off.
type connLimiter struct {
	max      int
	maxPerIP int

	mu    sync.Mutex
	total int
	perIP map[netip.Prefix]int
}

func newConnLimiter(maxConns, maxPerIP int) *connLimiter {
	return &connLimiter{
		max:      maxConns,
		maxPerIP: maxPerIP,
		perIP:    make(map[netip.Prefix]int),
	}
}

// clientKey returns the key a client address is counted under: the address
// itself for IPv4, and its /64 for IPv6, since a single client usually holds
// a whole /64. ok is false when addr carries no IP, such as a pipe in tests;
// such connections only count towards the total.
func clientKey(addr net.Addr) (netip.Prefix, bool) {
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	default:
		if addr == nil {
			return netip.Prefix{}, false
		}
		ap, err := netip.ParseAddrPort(addr.String())
		if err != nil {
			return netip.Prefix{}, false
		}
		ip = ap.Addr()
	}
	if !ip.IsValid() {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	p, err := ip.Prefix(bits)
	if err != nil {
		return netip.Prefix{}, false
	}
	return p, true
}

// acquire reserves a place for a connection from addr. It returns a release
// function, or nil when a limit is reached.
func (l *connLimiter) acquire(addr net.Addr) func() {
	key, hasKey := clientKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.total >= l.max {
		return nil
	}
	if hasKey && l.maxPerIP > 0 && l.perIP[key] >= l.maxPerIP {
		return nil
	}
	l.total++
	if hasKey {
		l.perIP[key]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if hasKey {
				if l.perIP[key] <= 1 {
					delete(l.perIP, key)
				} else {
					l.perIP[key]--
				}
			}
		})
	}
}

// limitedConn releases its place in the connection limiter when closed.
type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// sessionCounterKey is the connection context key of the number of open
// session channels on that connection.
type sessionCounterKey struct{}

// connCallback returns the ssh.ConnCallback that enforces the connection
// limits and sets up the per-connection session counter.
func connCallback(l *connLimiter, logger *log.Logger) ssh.ConnCallback {
	return func(ctx ssh.Context, conn net.Conn) net.Conn {
		release := l.acquire(conn.RemoteAddr())
		if release == nil {
			logger.Info("refusing connection: too many connections", "remote-addr", conn.RemoteAddr())
			return nil
		}
		ctx.SetValue(sessionCounterKey{}, new(atomic.Int64))
		return &limitedConn{Conn: conn, release: release}
	}
}

// limitSessions wraps a session channel handler so that a connection holds at
// most maxSessions session channels at once, as OpenSSH's MaxSessions does.
// Further channels are rejected until one closes. A zero limit returns next
// unchanged.
func limitSessions(next ssh.ChannelHandler, maxSessions int) ssh.ChannelHandler {
	if maxSessions <= 0 {
		return next
	}
	return func(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
		n, _ := ctx.Value(sessionCounterKey{}).(*atomic.Int64)
		if n == nil {
			next(srv, conn, newChan, ctx)
			return
		}
		if n.Add(1) > int64(maxSessions) {
			n.Add(-1)
			_ = newChan.Reject(gossh.ResourceShortage, "too many sessions on this connection")
			return
		}
		defer n.Add(-1)
		next(srv, conn, newChan, ctx)
	}
}
