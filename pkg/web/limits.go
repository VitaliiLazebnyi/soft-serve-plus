package web

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/charmbracelet/soft-serve/internal/clientaddr"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
)

// requestLimits bounds how many git and LFS requests the HTTP server serves
// at once, as the git daemon bounds its connections: in total, and from one
// client address, so a single client cannot take every slot and turn all
// other clients away. The zero value sets no limit.
type requestLimits struct {
	// max is the number of requests served at once, or 0 for no limit.
	max int
	// maxPerAddr is the number of requests served at once from one client
	// address (an IPv4 address, or an IPv6 /64), or 0 for no limit.
	maxPerAddr int

	mu      sync.Mutex
	total   int
	perAddr map[netip.Prefix]int
}

// newRequestLimits returns the limits cfg sets. A zero value means no limit.
func newRequestLimits(cfg config.HTTPConfig) *requestLimits {
	return &requestLimits{
		max:        cfg.MaxConnections,
		maxPerAddr: cfg.MaxConnectionsPerIP,
	}
}

// clientPrefix returns the key a request from remoteAddr, as in
// http.Request.RemoteAddr, is counted under against maxPerAddr; see
// clientaddr.Prefix. ok is false when remoteAddr holds no IP address, or a
// loopback one; such a request only counts towards the total. A reverse
// proxy on the same host connects from loopback, and every client it
// forwards would otherwise share one address's share.
func clientPrefix(remoteAddr string) (netip.Prefix, bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil || ap.Addr().Unmap().IsLoopback() {
		return netip.Prefix{}, false
	}
	return clientaddr.Prefix(ap.Addr())
}

// acquire takes a slot for a request from remoteAddr, and reports whether
// one was free. A request over the share of its address takes no slot of the
// total. release gives the slot back.
func (l *requestLimits) acquire(remoteAddr string) (release func(), ok bool) {
	key, keyed := clientPrefix(remoteAddr)
	keyed = keyed && l.maxPerAddr > 0
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.total >= l.max {
		return nil, false
	}
	if keyed && l.perAddr[key] >= l.maxPerAddr {
		return nil, false
	}
	l.total++
	if keyed {
		if l.perAddr == nil {
			l.perAddr = make(map[netip.Prefix]int)
		}
		l.perAddr[key]++
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		if keyed {
			if l.perAddr[key]--; l.perAddr[key] == 0 {
				delete(l.perAddr, key)
			}
		}
	}, true
}

// serve runs next within the limits. A request over a limit is turned away
// with 503 Service Unavailable, as the git daemon turns away a connection
// over its limit.
func (l *requestLimits) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	release, ok := l.acquire(r.RemoteAddr)
	if !ok {
		w.Header().Set("Retry-After", "1")
		if r.ProtoMajor == 1 {
			// Without this, the HTTP/1 server reads the body the
			// request announced before it sends the reply, for up to
			// the idle timeout.
			w.Header().Set("Connection", "close")
		}
		http.Error(w, gitserve.ErrMaxConnections.Error(), http.StatusServiceUnavailable)
		return
	}
	defer release()
	next.ServeHTTP(w, r)
}

// requestDeadlines bounds every request the HTTP server serves: how long one
// may make no progress reading its body or writing its response, and how
// long one may take in all. It covers every route, not only the git ones:
// net/http reads the body a request announced and its handler left unread
// before it answers, and without a deadline a client that announces a body
// and never sends it holds its connection for ever.
type requestDeadlines struct {
	idle time.Duration
	max  time.Duration
}

// withDeadlines returns h bound by the deadlines cfg sets. A zero value
// means no limit.
func withDeadlines(cfg config.HTTPConfig, h http.Handler) http.Handler {
	l := &requestDeadlines{
		idle: time.Duration(cfg.IdleTimeout) * time.Second,
		max:  time.Duration(cfg.MaxTimeout) * time.Second,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.serve(w, r, h)
	})
}

// serve runs next within the deadlines.
func (l *requestDeadlines) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if l.idle <= 0 && l.max <= 0 {
		next.ServeHTTP(w, r)
		return
	}

	d := &deadlines{
		rc:   http.NewResponseController(w),
		idle: l.idle,
	}
	ctx := r.Context()
	if l.max > 0 {
		d.max = time.Now().Add(l.max)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d.max)
		defer cancel()
	}

	r = r.WithContext(ctx)
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &deadlineBody{ReadCloser: r.Body, d: d}
	}
	// The server resets the deadlines before reading the next request on
	// the connection.
	defer d.extend()
	next.ServeHTTP(&deadlineWriter{ResponseWriter: w, d: d}, r)
}

// deadlines bounds each read of the request body and each write of the
// response, so a client that stops sending its body or stops reading the
// response is cut off after the idle timeout, and no request outlives max.
//
// Only a read or write in progress is bound by the idle timeout: when one
// returns, its deadline goes back to max (or none). The time git spends
// working between reads and writes is therefore not limited by the idle
// timeout; max still applies. Leaving the idle deadline armed would not do:
// HTTP/2 resets the stream when a deadline passes even with no read or write
// in progress, and the HTTP/1 server keeps reading the connection once the
// body is read.
type deadlines struct {
	rc   *http.ResponseController
	idle time.Duration
	// max is when the request must be done, or zero for no limit.
	max time.Time

	// mu guards done, reading, writing and every deadline set through rc.
	mu sync.Mutex
	// reading and writing count the reads of the body and the writes of
	// the response in progress.
	reading, writing int
	// done is set once the handler has returned. The connection then
	// belongs to the server again: over HTTP/2 the stream's state is
	// gone, and setting a deadline through rc panics; over HTTP/1 it
	// would undo the deadline the server set for the next request. A
	// goroutine the handler left reading the body (an LFS or other
	// handler's leftover reader) may still return from a read after
	// that.
	done bool
}

// next returns the deadline for a read or write starting now.
func (d *deadlines) next() time.Time {
	if d.idle <= 0 {
		return d.max
	}
	t := time.Now().Add(d.idle)
	if !d.max.IsZero() && t.After(d.max) {
		return d.max
	}
	return t
}

// extend sets both deadlines for the reads and writes the server does after
// the handler returns: discarding a body the handler did not read, and
// flushing what it wrote.
//
// It must be called before the handler returns; after it, no deadline is set
// through rc any more.
func (d *deadlines) extend() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.done = true
	t := d.next()
	_ = d.rc.SetReadDeadline(t)
	_ = d.rc.SetWriteDeadline(t)
}

// setRead sets the read deadline to t, unless the handler has returned. The
// caller holds mu, so the handler cannot return while it runs.
func (d *deadlines) setRead(t time.Time) {
	if !d.done {
		_ = d.rc.SetReadDeadline(t)
	}
}

// setWrite is setRead for the write deadline.
func (d *deadlines) setWrite(t time.Time) {
	if !d.done {
		_ = d.rc.SetWriteDeadline(t)
	}
}

// read runs a read of the body bound by the idle deadline.
func (d *deadlines) read(f func()) {
	d.mu.Lock()
	d.reading++
	d.setRead(d.next())
	d.mu.Unlock()

	f()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.reading--
	if d.reading > 0 {
		return
	}
	if d.writing > 0 {
		// A write in progress may be reading the body; see write.
		d.setRead(d.next())
	} else {
		d.setRead(d.max)
	}
}

// write runs a write of the response bound by the idle deadline.
//
// The read deadline is bound by it too while no read of the body is in
// progress: unless full duplex is enabled, the HTTP/1 server reads and
// discards the body the handler has not read once the response header goes
// out, which is on the first write that fills its buffer or on a flush. A
// read in progress keeps the deadline it set, so a full-duplex handler that
// reads and writes at once is unaffected.
func (d *deadlines) write(f func()) {
	d.mu.Lock()
	d.writing++
	t := d.next()
	d.setWrite(t)
	if d.reading == 0 {
		d.setRead(t)
	}
	d.mu.Unlock()

	f()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.writing--
	if d.writing == 0 {
		d.setWrite(d.max)
		if d.reading == 0 {
			d.setRead(d.max)
		}
	}
}

// deadlineBody is a request body whose reads are bound by the idle deadline.
type deadlineBody struct {
	io.ReadCloser
	d *deadlines
}

// Read implements io.Reader.
func (b *deadlineBody) Read(p []byte) (n int, err error) {
	b.d.read(func() { n, err = b.ReadCloser.Read(p) })
	return n, err
}

// deadlineWriter is a response writer whose writes are bound by the idle
// deadline.
type deadlineWriter struct {
	http.ResponseWriter
	d *deadlines
}

// Write implements http.ResponseWriter.
func (w *deadlineWriter) Write(p []byte) (n int, err error) {
	w.d.write(func() { n, err = w.ResponseWriter.Write(p) })
	return n, err
}

// Flush implements http.Flusher.
func (w *deadlineWriter) Flush() {
	w.d.write(func() { _ = w.d.rc.Flush() })
}

// Unwrap returns the underlying http.ResponseWriter, for
// http.ResponseController.
func (w *deadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
