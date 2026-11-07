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
	bm "charm.land/wish/v2/bubbletea"
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
}

// NewSSHServer returns a new SSHServer.
func NewSSHServer(ctx context.Context) (*SSHServer, error) {
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
			bm.MiddlewareWithProgramHandler(SessionHandler),
			// CLI middleware.
			CommandMiddleware,
			// Logging middleware.
			LoggingMiddleware,
			// Authentication middleware.
			// charm.land/ssh already gives the session the key that
			// authenticated it. This checks it against the fingerprint
			// PublicKeyHandler stored, as defense in depth, and sets the
			// session's user.
			AuthenticationMiddleware,
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
// those in the config, which default to the algorithms x/crypto considers
// secure. Without them x/crypto falls back to its own defaults, which still
// include SHA-1 key exchange and MACs. The SSH server adds the host keys
// and authentication callbacks on top of this.
func serverConfig(cfg *config.Config, logger *log.Logger) *gossh.ServerConfig {
	sc := &gossh.ServerConfig{
		Config: gossh.Config{
			KeyExchanges: cfg.SSH.SSHKexAlgorithms(),
			Ciphers:      cfg.SSH.SSHCiphers(),
			MACs:         cfg.SSH.SSHMACs(),
		},
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

// initializePermissions returns the context's permissions, creating them
// and their extensions map when missing.
func initializePermissions(ctx ssh.Context) *ssh.Permissions {
	perms := ctx.Permissions()
	if perms == nil || perms.Permissions == nil {
		perms = &ssh.Permissions{Permissions: &gossh.Permissions{}}
	}
	if perms.Extensions == nil {
		perms.Extensions = make(map[string]string)
	}
	return perms
}

// PublicKeyHandler handles public key authentication.
func (s *SSHServer) PublicKeyHandler(ctx ssh.Context, pk ssh.PublicKey) (allowed bool) {
	if pk == nil {
		return false
	}

	allowed = true

	// Store the offered key's fingerprint in the permissions block for
	// AuthenticationMiddleware to check. Each attempt overwrites it, so it
	// ends up naming the key that authenticated.
	perms := initializePermissions(ctx)

	// Set the public key fingerprint to be used for authentication.
	perms.Extensions["pubkey-fp"] = gossh.FingerprintSHA256(pk)
	ctx.SetValue(ssh.ContextKeyPermissions, perms)

	return
}

// KeyboardInteractiveHandler handles keyboard interactive authentication,
// which lets a client in without a key when keyless access is allowed.
func (s *SSHServer) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	ac := s.be.AllowKeyless(ctx)
	keyboardInteractiveCounter.WithLabelValues(strconv.FormatBool(ac)).Inc()

	perms := initializePermissions(ctx)
	if ac {
		// Clear the fingerprint a failed public key attempt may have left,
		// since no key authenticated this session.
		perms.Extensions["pubkey-fp"] = ""
		ctx.SetValue(ssh.ContextKeyPermissions, perms)
	}
	return ac
}
