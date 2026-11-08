package web

import (
	"context"
	"crypto/tls"
	stdlog "log"
	"net"
	"net/http"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
)

// HTTPServer is an http server.
type HTTPServer struct {
	ctx context.Context
	cfg *config.Config

	Server *http.Server

	// pending tracks the repositories HTTP pushes created. It is closed
	// with the server so no removal of one outlives it.
	pending *pushCreates
}

// NewHTTPServer creates a new HTTP server.
func NewHTTPServer(ctx context.Context) (*HTTPServer, error) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx)
	pending := newPushCreates()
	s := &HTTPServer{
		ctx:     ctx,
		cfg:     cfg,
		pending: pending,
		Server: &http.Server{
			Addr:              cfg.HTTP.ListenAddr,
			Handler:           newRouter(ctx, pending),
			ReadHeaderTimeout: time.Second * 10,
			IdleTimeout:       time.Second * 10,
			MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
			ErrorLog:          stdlog.New(serverErrorLog{logger.WithPrefix("http")}, "", 0),
		},
	}

	return s, nil
}

// serverErrorLog is the error log of the HTTP server. Clients cause most
// failed TLS handshakes, by scanning the port or closing the connection, so
// those are logged at debug level; anything else is an error.
type serverErrorLog struct {
	logger *log.Logger
}

// Write implements io.Writer.
func (l serverErrorLog) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if strings.HasPrefix(msg, "http: TLS handshake error") {
		l.logger.Debug(msg)
	} else {
		l.logger.Error(msg)
	}
	return len(p), nil
}

// SetTLSConfig sets the TLS configuration for the HTTP server.
func (s *HTTPServer) SetTLSConfig(tlsConfig *tls.Config) {
	s.Server.TLSConfig = tlsConfig
}

// Close closes the HTTP server. It stops the removals of repositories HTTP
// pushes created that are due later, and waits for the ones running.
func (s *HTTPServer) Close() error {
	err := s.Server.Close()
	_ = s.pending.close(context.Background())
	return err
}

// ListenAndServe starts the HTTP server.
func (s *HTTPServer) ListenAndServe() error {
	if s.Server.TLSConfig != nil {
		return s.Server.ListenAndServeTLS("", "")
	}
	return s.Server.ListenAndServe()
}

// Serve serves the HTTP server on l, with TLS when a TLS configuration is set.
func (s *HTTPServer) Serve(l net.Listener) error {
	if s.Server.TLSConfig != nil {
		return s.Server.ServeTLS(l, "", "")
	}
	return s.Server.Serve(l)
}

// Shutdown gracefully shuts down the HTTP server. Once the requests have
// drained, it stops the removals of repositories HTTP pushes created that
// are due later, and waits for the ones running, within ctx.
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	err := s.Server.Shutdown(ctx)
	if cerr := s.pending.close(ctx); err == nil {
		err = cerr
	}
	return err
}
