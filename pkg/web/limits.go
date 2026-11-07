package web

import (
	"context"
	"errors"
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
	// The deadlines cover the rest of the request too: discarding a body
	// the handler did not read, and flushing what it wrote. The server
	// resets them before reading the next request on the connection.
	defer d.extend()
	next.ServeHTTP(&deadlineWriter{ResponseWriter: w, d: d}, r)
}

// deadlines moves the connection's read and write deadlines forward as a
// request makes progress, so a client that stops sending its body or stops
// reading the response is cut off after the idle timeout, and no request
// outlives max.
//
// Only a read or write in progress is bound by a deadline, so the time git
// spends working between reads and writes is not limited by the idle
// timeout; max still applies.
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

// extend sets both deadlines for a read or write starting now.
func (d *deadlines) extend() {
	t := d.next()
	_ = d.rc.SetReadDeadline(t)
	_ = d.rc.SetWriteDeadline(t)
}

// deadlineBody is a request body that extends the read deadline before each
// read.
type deadlineBody struct {
	io.ReadCloser
	d *deadlines
}

// Read implements io.Reader.
func (b *deadlineBody) Read(p []byte) (int, error) {
	_ = b.d.rc.SetReadDeadline(b.d.next())
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		// Once the body is read, the HTTP/1 server keeps reading the
		// connection to notice the client going away, and a deadline
		// passing there would cancel the request while git is still
		// working. Only max applies from here on.
		_ = b.d.rc.SetReadDeadline(b.d.max)
	}
	return n, err
}

// deadlineWriter is a response writer that extends the write deadline before
// each write.
type deadlineWriter struct {
	http.ResponseWriter
	d *deadlines
}

// Write implements http.ResponseWriter.
func (w *deadlineWriter) Write(p []byte) (int, error) {
	_ = w.d.rc.SetWriteDeadline(w.d.next())
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher.
func (w *deadlineWriter) Flush() {
	_ = w.d.rc.SetWriteDeadline(w.d.next())
	_ = w.d.rc.Flush()
}

// Unwrap returns the underlying http.ResponseWriter, for
// http.ResponseController.
func (w *deadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
