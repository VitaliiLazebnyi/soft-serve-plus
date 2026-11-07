package web

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
)

// requestLimits bounds the git and LFS requests the HTTP server serves, as
// the git daemon bounds its connections: how many run at once, how long one
// may make no progress reading its body or writing its response, and how long
// one may take in all.
type requestLimits struct {
	// slots holds a token for every request being served. It is nil when
	// the number of requests is not limited.
	slots chan struct{}
	idle  time.Duration
	max   time.Duration
}

// newRequestLimits returns the limits cfg sets. A zero value means no limit.
func newRequestLimits(cfg config.HTTPConfig) *requestLimits {
	l := &requestLimits{
		idle: time.Duration(cfg.IdleTimeout) * time.Second,
		max:  time.Duration(cfg.MaxTimeout) * time.Second,
	}
	if cfg.MaxConnections > 0 {
		l.slots = make(chan struct{}, cfg.MaxConnections)
	}
	return l
}

// serve runs next within the limits. A request over the concurrency limit is
// turned away with 503 Service Unavailable, as the git daemon turns away a
// connection over its limit.
func (l *requestLimits) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if l.slots != nil {
		select {
		case l.slots <- struct{}{}:
			defer func() { <-l.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			if r.ProtoMajor == 1 {
				// Without this, the HTTP/1 server reads the body
				// the request announced before it sends the reply,
				// and waits with no deadline for a client that
				// stalls.
				w.Header().Set("Connection", "close")
			}
			http.Error(w, git.ErrMaxConnections.Error(), http.StatusServiceUnavailable)
			return
		}
	}

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
func (d *deadlines) extend() {
	t := d.next()
	_ = d.rc.SetReadDeadline(t)
	_ = d.rc.SetWriteDeadline(t)
}

// read runs a read of the body bound by the idle deadline.
func (d *deadlines) read(f func()) {
	_ = d.rc.SetReadDeadline(d.next())
	f()
	_ = d.rc.SetReadDeadline(d.max)
}

// write runs a write of the response bound by the idle deadline.
func (d *deadlines) write(f func()) {
	_ = d.rc.SetWriteDeadline(d.next())
	f()
	_ = d.rc.SetWriteDeadline(d.max)
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
