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
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// HTTPServer is an http server.
type HTTPServer struct {
	Server *http.Server

	// pending tracks the repositories HTTP pushes created. It is closed
	// with the server so no removal of one outlives it.
	pending *pushCreates
}

// maxHeaderBytes bounds the request line and headers the server reads. git,
// git-lfs and go send short ones: a path, a few headers and at most a token.
// The net/http default of 1 MB would let an anonymous request hand every
// handler a path of nearly that length.
const maxHeaderBytes = 64 << 10

// NewHTTPServer returns the HTTP server cfg sets up, serving the
// repositories of be, whose database and store are dbx and datastore.
// tokens verifies bearer tokens; a nil one, for a server without a usable
// host key, refuses them all. A nil logger is log.Default().
func NewHTTPServer(cfg *config.Config, be *backend.Backend, dbx *db.DB, datastore store.Store, tokens *jwk.Signer, logger *log.Logger) (*HTTPServer, error) {
	if cfg == nil {
		return nil, config.ErrNilConfig
	}
	s := newServer(cfg, be, dbx, datastore, tokens, logger)
	return &HTTPServer{
		pending: s.pending,
		Server: &http.Server{
			Addr:              cfg.HTTP.ListenAddr,
			Handler:           s.router(),
			ReadHeaderTimeout: time.Second * 10,
			IdleTimeout:       time.Second * 10,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          stdlog.New(serverErrorLog{s.logger}, "", 0),
		},
	}, nil
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
