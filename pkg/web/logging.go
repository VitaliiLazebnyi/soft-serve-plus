package web

import (
	"fmt"
	"net/http"
	"time"

	"charm.land/log/v2"
	"github.com/dustin/go-humanize"
)

// logWriter is a wrapper around http.ResponseWriter that allows us to capture
// the HTTP status code and bytes written to the response.
type logWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
}

var (
	_ http.ResponseWriter = (*logWriter)(nil)
	_ http.Flusher        = (*logWriter)(nil)
)

// Write implements http.ResponseWriter.
func (r *logWriter) Write(p []byte) (int, error) {
	written, err := r.ResponseWriter.Write(p)
	r.bytes += int64(written)
	return written, err
}

// Note this is generally only called when sending an HTTP error, so it's
// important to set the `code` value to 200 as a default.
func (r *logWriter) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the underlying http.ResponseWriter.
func (r *logWriter) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush implements http.Flusher.
func (r *logWriter) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// newLoggingMiddleware returns a new logging middleware.
func newLoggingMiddleware(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		writer := &logWriter{code: http.StatusOK, ResponseWriter: w}
		logger.Debug("request",
			"method", r.Method,
			"path", r.URL,
			"addr", r.RemoteAddr)
		next.ServeHTTP(writer, r)
		elapsed := time.Since(start)
		logger.Debug("response",
			"status", fmt.Sprintf("%d %s", writer.code, http.StatusText(writer.code)),
			"bytes", humanize.Bytes(uint64(writer.bytes)), //nolint:gosec
			"time", elapsed)
	})
}
