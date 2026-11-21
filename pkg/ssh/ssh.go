package ssh

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	rm "charm.land/wish/v2/recover"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	gossh "golang.org/x/crypto/ssh"
)

var (
	publicKeyCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "public_key_auth_total",
		Help:      "The total number of public key auth requests",
	}, []string{"allowed"})

	keyboardInteractiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "keyboard_interactive_auth_total",
		Help:      "The total number of keyboard interactive auth requests",
	}, []string{"allowed"})
)

// SSHServer is a SSH server that implements the git protocol.
type SSHServer struct { //nolint: revive
	srv *ssh.Server
	be  *backend.Backend

	limiter *connLimiter
}

// serverLimits are the fixed per-connection limits of the SSH server.
type serverLimits struct {
	// handshakeTimeout bounds the handshake and authentication.
	handshakeTimeout time.Duration
	// maxSessions bounds the sessions open at once on one connection.
	maxSessions int
}

// NewSSHServer returns a new SSHServer.
func NewSSHServer(ctx context.Context) (*SSHServer, error) {
	return newSSHServer(ctx, serverLimits{
		handshakeTimeout: config.SSHHandshakeTimeout,
		maxSessions:      config.SSHMaxSessions,
	})
}

// newSSHServer returns a new SSHServer with lim, which tests lower.
func newSSHServer(ctx context.Context, lim serverLimits) (*SSHServer, error) {
	cfg := config.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("ssh")
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	be := backend.FromContext(ctx)

	var err error
	s := &SSHServer{be: be}

	mw := []wish.Middleware{
		rm.MiddlewareWithLogger(
			logger,
			// BubbleTea middleware.
			BubbleTeaMiddleware,
			// CLI middleware.
			CommandMiddleware,
			// Logging middleware.
			LoggingMiddleware,
			// Authentication middleware.
			// Sets the session's user from the key that authenticated
			// it, and refuses keyless sessions when keyless access is off.
			AuthenticationMiddleware,
			// Command length middleware.
			// Refuses overlong exec commands before anything parses them.
			CommandLengthMiddleware,
			// Context middleware.
			// This must come first to set up the context.
			ContextMiddleware(cfg, dbx, datastore, be, logger),
		),
	}

	opts := []ssh.Option{
		ssh.PublicKeyAuth(s.PublicKeyHandler),
		ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler),
		wish.WithAddress(cfg.SSH.ListenAddr),
		wish.WithHostKeyPath(cfg.SSH.KeyPath),
		wish.WithMiddleware(mw...),
	}

	// TODO: Support a real PTY in future version.
	opts = append(opts, ssh.EmulatePty())

	s.srv, err = wish.NewServer(opts...)
	if err != nil {
		return nil, err
	}

	s.srv.ServerConfigCallback = func(_ ssh.Context) *gossh.ServerConfig {
		return serverConfig(cfg, logger)
	}

	if cfg.SSH.MaxTimeout > 0 {
		s.srv.MaxTimeout = time.Duration(cfg.SSH.MaxTimeout) * time.Second
	}

	if cfg.SSH.IdleTimeout > 0 {
		s.srv.IdleTimeout = time.Duration(cfg.SSH.IdleTimeout) * time.Second
	}

	// Close connections that have not finished the handshake and
	// authenticated in time. A connection holds its place under the
	// connection limits from the moment it is accepted, so without this an
	// unauthenticated client could hold places for the whole idle timeout,
	// or for ever by sending a byte now and then.
	s.srv.HandshakeTimeout = lim.handshakeTimeout

	// Limit the connections open at once, in total and per client address,
	// and the session channels and running commands each connection may
	// hold. Session channels pass only the requests the server can afford
	// (see filterSessionRequests).
	s.limiter = newConnLimiter(cfg.SSH.MaxConnections, cfg.SSH.MaxConnectionsPerIP)
	s.srv.ConnCallback = connCallback(s.limiter, logger, noAccountLifetime)
	setSessionHandlers(s.srv, lim.maxSessions)

	// Create client ssh key
	if _, err := os.Stat(cfg.SSH.ClientKeyPath); errors.Is(err, fs.ErrNotExist) {
		_, err := keygen.New(cfg.SSH.ClientKeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite())
		if err != nil {
			return nil, fmt.Errorf("client ssh key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("client ssh key: %w", err)
	}

	return s, nil
}

// serverConfig returns the x/crypto server configuration for a new
// connection. It restricts the key exchanges, ciphers and MACs offered to
// those x/crypto considers secure, without hmac-sha1, and the signature
// algorithms accepted for public key authentication to those in the config,
// which default to the secure ones too. Without them x/crypto falls back to
// its own defaults, which still include SHA-1 key exchange and MACs and
// accept ssh-rsa and ssh-dss signatures. The SSH server adds the host keys
// and authentication callbacks on top of this.
func serverConfig(cfg *config.Config, logger *log.Logger) *gossh.ServerConfig {
	sc := &gossh.ServerConfig{
		Config: gossh.Config{
			KeyExchanges: config.DefaultSSHKexAlgorithms(),
			Ciphers:      config.DefaultSSHCiphers(),
			MACs:         config.DefaultSSHMACs(),
		},
		PublicKeyAuthAlgorithms: cfg.SSH.SSHPublicKeyAuthAlgorithms(),
	}
	if config.IsDebug() {
		sc.AuthLogCallback = func(conn gossh.ConnMetadata, method string, err error) {
			logger.Debug("authentication", "user", conn.User(), "method", method, "err", err)
		}
	}
	return sc
}

// ListenAndServe starts the SSH server.
func (s *SSHServer) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Serve starts the SSH server on the given net.Listener.
func (s *SSHServer) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Close closes the SSH server.
func (s *SSHServer) Close() error {
	return s.srv.Close()
}

// Shutdown gracefully shuts down the SSH server.
func (s *SSHServer) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// PublicKeyHandler handles public key authentication. It approves every key:
// what a key may do is decided per session, from the account that owns it.
// charm.land/ssh gives each session the key that signed, which is how
// AuthenticationMiddleware finds the session's user.
func (s *SSHServer) PublicKeyHandler(_ ssh.Context, pk ssh.PublicKey) (allowed bool) {
	allowed = pk != nil
	publicKeyCounter.WithLabelValues(strconv.FormatBool(allowed)).Inc()
	return allowed
}

// KeyboardInteractiveHandler handles keyboard interactive authentication,
// which lets a client in without a key when keyless access is allowed.
func (s *SSHServer) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	ac := s.be.AllowKeyless(ctx)
	keyboardInteractiveCounter.WithLabelValues(strconv.FormatBool(ac)).Inc()
	return ac
}
