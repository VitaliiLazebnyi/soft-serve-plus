package ssh

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"charm.land/log/v2"
	"charm.land/ssh"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// Reasons a connection is refused, used as the label of
// refusedConnectionCounter. The label only ever takes these fixed values,
// never anything the client sends.
const (
	refusedTotal = "max_connections"
	refusedPerIP = "max_connections_per_ip"
)

var refusedConnectionCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "soft_serve",
	Subsystem: "ssh",
	Name:      "refused_connections_total",
	Help:      "The total number of SSH connections refused by a connection limit",
}, []string{"limit"})

// acquire reserves a place for a connection from addr. It returns a release
// function, or nil and the limit that was reached.
func (l *connLimiter) acquire(addr net.Addr) (func(), string) {
	key, hasKey := clientKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.total >= l.max {
		return nil, refusedTotal
	}
	if hasKey && l.maxPerIP > 0 && l.perIP[key] >= l.maxPerIP {
		return nil, refusedPerIP
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
	}, ""
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
		release, limit := l.acquire(conn.RemoteAddr())
		if release == nil {
			// A flood of connections is what these limits stop, so a
			// refusal is counted, and logged only at debug level, to keep
			// the flood out of the log.
			refusedConnectionCounter.WithLabelValues(limit).Inc()
			logger.Debug("refusing connection: too many connections", "limit", limit, "remote-addr", conn.RemoteAddr())
			return nil
		}
		ctx.SetValue(sessionCounterKey{}, new(atomic.Int64))
		return &limitedConn{Conn: conn, release: release}
	}
}

// limitSessions wraps a session channel handler so that a connection holds at
// most maxSessions sessions at once, as OpenSSH's MaxSessions does. Further
// channels are rejected until one ends. A zero limit returns next unchanged.
//
// A session holds its place until both its channel's request loop and the
// handler it started, if any, have returned. ssh.DefaultSessionHandler runs
// the shell, exec or subsystem handler in a goroutine of its own and returns
// as soon as the client closes the channel, so freeing the place then would
// let a client start a command, close the channel at once and repeat,
// keeping any number of commands running on one connection.
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
		var running sync.WaitGroup
		next(trackHandlers(srv, &running), conn, newChan, ctx)
		running.Wait()
	}
}

// trackHandlers returns a server for a single session channel that hands that
// channel's handlers to ssh.DefaultSessionHandler as srv does, but counts the
// one it starts in running until it returns.
//
// The handler starts in a goroutine that may not have run by the time the
// request loop returns, so the count goes up in the session request callback,
// which the request loop calls before it starts a handler; the request loop
// accepts at most one shell, exec or subsystem request per session. The
// returned server only carries the fields ssh.DefaultSessionHandler reads:
// copying the whole of srv would copy its locks.
func trackHandlers(srv *ssh.Server, running *sync.WaitGroup) *ssh.Server {
	t := &ssh.Server{
		PtyCallback: srv.PtyCallback,
		PtyHandler:  srv.PtyHandler,
	}
	if srv.Handler != nil {
		h := srv.Handler
		t.Handler = func(s ssh.Session) {
			defer running.Done()
			h(s)
		}
	}
	if srv.SubsystemHandlers != nil {
		t.SubsystemHandlers = make(map[string]ssh.SubsystemHandler, len(srv.SubsystemHandlers))
		for name, h := range srv.SubsystemHandlers {
			if h == nil {
				continue
			}
			t.SubsystemHandlers[name] = func(s ssh.Session) {
				defer running.Done()
				h(s)
			}
		}
	}
	cb := srv.SessionRequestCallback
	t.SessionRequestCallback = func(s ssh.Session, requestType string) bool {
		if cb != nil && !cb(s, requestType) {
			return false
		}
		// Count the handler only when the request loop is about to start
		// one, mirroring its own checks after this callback.
		switch requestType {
		case "shell", "exec":
			if t.Handler != nil {
				running.Add(1)
			}
		case "subsystem":
			if t.SubsystemHandlers[s.Subsystem()] != nil || t.SubsystemHandlers["default"] != nil {
				running.Add(1)
			}
		}
		return true
	}
	return t
}
