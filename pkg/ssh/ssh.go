package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"charm.land/log/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	rm "charm.land/wish/v2/recover"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/ssh/cmd"
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

// Server is an SSH server that implements the git protocol.
type Server struct {
	srv *ssh.Server
	cfg *config.Config
	be  *backend.Backend
	// logger is the server's logger, with the "ssh" prefix.
	logger *log.Logger
	// cmds builds the commands of each session.
	cmds *cmd.Commands

	limiter *connLimiter
	// live holds every connection accepted and not yet closed, the ones
	// still in their handshake included (see liveConns).
	live liveConns
}

// serverLimits are the fixed per-connection limits of the SSH server.
type serverLimits struct {
	// handshakeTimeout bounds the handshake and authentication.
	handshakeTimeout time.Duration
	// maxSessions bounds the sessions open at once on one connection.
	maxSessions int
}

// errNilHostKey is returned by NewServer without a host key.
var errNilHostKey = errors.New("nil host key")

// NewServer returns an SSH server for cfg and be, which presents hostKey
// to clients and signs the tokens of `jwt` and git-lfs-authenticate with
// tokens. tokens must be the Signer the HTTP server verifies with, or the
// HTTP server refuses those tokens. dbx and st are for git-lfs-transfer.
// A nil logger means log.Default().
func NewServer(cfg *config.Config, dbx *db.DB, st store.Store, be *backend.Backend, hostKey *config.HostKey, tokens *jwk.Signer, logger *log.Logger) (*Server, error) {
	return newServer(cfg, dbx, st, be, hostKey, tokens, logger, serverLimits{
		handshakeTimeout: config.SSHHandshakeTimeout,
		maxSessions:      config.SSHMaxSessions,
	})
}

// newServer is NewServer with lim, which tests lower.
func newServer(cfg *config.Config, dbx *db.DB, st store.Store, be *backend.Backend, hostKey *config.HostKey, tokens *jwk.Signer, logger *log.Logger, lim serverLimits) (*Server, error) {
	if cfg == nil {
		return nil, config.ErrNilConfig
	}
	if hostKey == nil {
		return nil, errNilHostKey
	}
	if logger == nil {
		logger = log.Default()
	}
	logger = logger.WithPrefix("ssh")
	signer, err := gossh.NewSignerFromKey(hostKey.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}
	cmds, err := cmd.New(cmd.Deps{
		Config:  cfg,
		Backend: be,
		DB:      dbx,
		Store:   st,
		Tokens:  tokens,
		Logger:  logger,
	})
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, be: be, logger: logger, cmds: cmds}

	mw := []wish.Middleware{
		rm.MiddlewareWithLogger(
			logger,
			// BubbleTea middleware.
			s.bubbleTeaMiddleware,
			// CLI middleware.
			s.commandMiddleware,
			// Logging middleware.
			s.loggingMiddleware,
			// Authentication middleware.
			// Sets the session's user from the key that authenticated
			// it, and refuses keyless sessions when keyless access is off.
			s.authenticationMiddleware,
			// Command length middleware.
			// Refuses overlong exec commands before anything parses them.
			commandLengthMiddleware,
			// Logger middleware.
			// This must come first to set up the context.
			s.loggerMiddleware,
		),
	}

	opts := []ssh.Option{
		ssh.PublicKeyAuth(s.PublicKeyHandler),
		ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler),
		wish.WithAddress(cfg.SSH.ListenAddr),
		func(srv *ssh.Server) error {
			srv.AddHostKey(signer)
			return nil
		},
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
	s.srv.ConnCallback = s.live.track(connCallback(s.limiter, logger, noAccountLifetime))
	setSessionHandlers(s.srv, lim.maxSessions)

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
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Serve starts the SSH server on the given net.Listener.
func (s *Server) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Close closes the SSH server.
//
// It closes every connection, the ones still in their handshake included,
// which the SSH library's Close does not know about yet.
func (s *Server) Close() error {
	err := s.srv.Close()
	s.live.closeAll()
	return err
}

// Shutdown gracefully shuts down the SSH server: it stops accepting
// connections, closes the ones still in their handshake, which have no work
// in flight, and waits within ctx for the others to end.
func (s *Server) Shutdown(ctx context.Context) error {
	s.live.closeUnestablished()
	return s.srv.Shutdown(ctx)
}

// liveConns holds the connections an SSH server accepted and has not yet
// closed. The SSH library tracks a connection, to close it on Close and wait
// for it on Shutdown, only once its handshake is done, and does not check
// then whether the server has stopped. So a connection whose handshake
// finished just after a stop was authenticated afterwards and stayed open
// until its idle timeout, the drain's cut-off included. Once the server
// stops, liveConns refuses new connections and closes the ones it holds.
// It is not reset, so a Server that has stopped cannot serve again.
type liveConns struct {
	mu      sync.Mutex
	conns   map[*liveConn]struct{}
	stopped bool
}

// liveConn is a connection liveConns holds. ctx is its SSH context, which
// the library gives the established connection once the handshake is done.
type liveConn struct {
	net.Conn
	ctx   ssh.Context
	owner *liveConns
	once  sync.Once
}

// Close closes the connection and lets go of it.
func (c *liveConn) Close() error {
	c.once.Do(c.owner.remove(c))
	return c.Conn.Close()
}

// established reports whether the connection has finished its handshake, so
// that the SSH library tracks it.
func (c *liveConn) established() bool {
	return c.ctx.Value(ssh.ContextKeyConn) != nil
}

// track wraps next, the server's connection callback, so that every
// connection it lets in is held, or refused once the server has stopped.
func (l *liveConns) track(next ssh.ConnCallback) ssh.ConnCallback {
	return func(ctx ssh.Context, conn net.Conn) net.Conn {
		c := next(ctx, conn)
		if c == nil {
			return nil
		}
		lc := &liveConn{Conn: c, ctx: ctx, owner: l}
		l.mu.Lock()
		if l.stopped {
			l.mu.Unlock()
			_ = c.Close()
			return nil
		}
		if l.conns == nil {
			l.conns = make(map[*liveConn]struct{})
		}
		l.conns[lc] = struct{}{}
		l.mu.Unlock()
		return lc
	}
}

// remove returns a function that lets go of c.
func (l *liveConns) remove(c *liveConn) func() {
	return func() {
		l.mu.Lock()
		delete(l.conns, c)
		l.mu.Unlock()
	}
}

// stop refuses new connections and returns the ones held that keep
// reports true for.
func (l *liveConns) stop(keep func(*liveConn) bool) []*liveConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	var conns []*liveConn
	for c := range l.conns {
		if !keep(c) {
			conns = append(conns, c)
		}
	}
	return conns
}

// closeAll refuses new connections and closes every one held.
func (l *liveConns) closeAll() {
	for _, c := range l.stop(func(*liveConn) bool { return false }) {
		_ = c.Close()
	}
}

// closeUnestablished refuses new connections and closes the ones still in
// their handshake. The established ones are the SSH library's to drain. A
// connection the library has just tracked but not yet given its context
// value counts as unestablished and is closed too; it has no channels yet.
func (l *liveConns) closeUnestablished() {
	for _, c := range l.stop((*liveConn).established) {
		_ = c.Close()
	}
}

// PublicKeyHandler handles public key authentication. It approves every key:
// what a key may do is decided per session, from the account that owns it.
// charm.land/ssh gives each session the key that signed, which is how
// AuthenticationMiddleware finds the session's user.
func (s *Server) PublicKeyHandler(_ ssh.Context, pk ssh.PublicKey) (allowed bool) {
	allowed = pk != nil
	publicKeyCounter.WithLabelValues(strconv.FormatBool(allowed)).Inc()
	return allowed
}

// KeyboardInteractiveHandler handles keyboard interactive authentication,
// which lets a client in without a key when keyless access is allowed.
func (s *Server) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	ac := s.be.AllowKeyless(ctx)
	keyboardInteractiveCounter.WithLabelValues(strconv.FormatBool(ac)).Inc()
	return ac
}
