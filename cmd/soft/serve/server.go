package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"charm.land/log/v2"

	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
	"github.com/charmbracelet/soft-serve/pkg/daemon"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/jobs"
	"github.com/charmbracelet/soft-serve/pkg/jwk"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	sshsrv "github.com/charmbracelet/soft-serve/pkg/ssh"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/web"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

// Server is the Soft Serve server.
type Server struct {
	SSHServer   *sshsrv.Server
	GitDaemon   *daemon.GitDaemon
	HTTPServer  *web.HTTPServer
	StatsServer *http.Server
	CertLoader  *CertReloader
	Cron        *cron.Scheduler
	Config      *config.Config
	Backend     *backend.Backend

	logger *log.Logger
	// jobCtx is the context the cron jobs run with, and stopJobs cancels it.
	jobCtx   context.Context
	stopJobs context.CancelFunc

	// mu guards listeners and closed. Start keeps the listeners it bound
	// so that Shutdown and Close can close them even if they run before the
	// servers have started serving on them.
	mu        sync.Mutex
	listeners []*serveListener
	closed    bool
}

// NewServer returns a new *Server configured to serve Soft Serve with cfg,
// the database dbx and store st, and the backend be built on them, logging
// on logger. The SSH host key and the client key are created if they do not
// exist. ctx is the server's lifetime: the cron jobs end with it, and the
// start-up checks run under it.
//
// A nil config is config.ErrNilConfig, and a nil database, store or backend
// an error; a nil logger is log.Default().
func NewServer(ctx context.Context, cfg *config.Config, dbx *db.DB, st store.Store, be *backend.Backend, logger *log.Logger) (*Server, error) {
	return newServer(ctx, cfg, dbx, st, be, logger, closeServer)
}

// newServer is NewServer, which closes a server it could not finish
// building with closeFailed: closeServer, or a test's that records it.
func newServer(ctx context.Context, cfg *config.Config, dbx *db.DB, st store.Store, be *backend.Backend, logger *log.Logger, closeFailed func(*Server)) (_ *Server, err error) {
	switch {
	case cfg == nil:
		return nil, config.ErrNilConfig
	case dbx == nil:
		return nil, errors.New("serve: nil database")
	case st == nil:
		return nil, errors.New("serve: nil store")
	case be == nil:
		return nil, errors.New("serve: nil backend")
	}
	if logger == nil {
		logger = log.Default()
	}
	baseLogger := logger
	logger = baseLogger.WithPrefix("server")
	// The cron jobs run with a context Shutdown and Close cancel, so a
	// running mirror sync starts no further git command or lfs request and
	// ends rather than outlive the server and the database. A git command
	// already running gets SIGTERM, on which git removes its ref locks (see
	// jobs.runMirrorGit).
	jobCtx, stopJobs := context.WithCancel(ctx)
	srv := &Server{
		Config:   cfg,
		Backend:  be,
		logger:   logger,
		jobCtx:   jobCtx,
		stopJobs: stopJobs,
	}

	// If a later step fails, close what was already built rather than leave
	// it to the caller, which only gets the error.
	defer func() {
		if err != nil {
			closeFailed(srv)
		}
	}()

	// Add cron jobs.
	sched := cron.NewScheduler(baseLogger)
	// The jobs set prefixes of their own; scheduleJobs logs as the server.
	if err := scheduleJobs(jobCtx, cfg, be, sched, logger); err != nil {
		return nil, err
	}

	srv.Cron = sched

	// Nothing serves requests yet, so no create, rename or delete is in
	// progress to leave a directory without its row for a moment.
	be.CheckRepositoryDirs(ctx)
	ensureDefaultRepo(ctx, cfg, be, logger)

	if err := ensureKey(cfg.SSH.ClientKeyPath, "client ssh key"); err != nil {
		return nil, err
	}

	// The host key is read once, here: the SSH server presents it, and it
	// signs and verifies the tokens (soft jwt, git-lfs-authenticate, HTTP
	// bearer) without reading the file on every request.
	if err := ensureKey(cfg.SSH.KeyPath, "ssh host key"); err != nil {
		return nil, err
	}
	hostKey, err := config.KeyPair(cfg)
	if err != nil {
		return nil, err
	}

	tokens, err := jwk.NewSigner(hostKey, cfg.HTTP.PublicURL)
	if err != nil {
		return nil, err
	}

	srv.SSHServer, err = sshsrv.NewServer(cfg, dbx, st, be, hostKey, tokens, baseLogger)
	if err != nil {
		return nil, fmt.Errorf("create ssh server: %w", err)
	}

	srv.GitDaemon, err = daemon.NewGitDaemon(cfg, be, baseLogger)
	if err != nil {
		return nil, fmt.Errorf("create git daemon: %w", err)
	}

	srv.HTTPServer, err = web.NewHTTPServer(cfg, be, dbx, st, tokens, baseLogger)
	if err != nil {
		return nil, fmt.Errorf("create http server: %w", err)
	}

	srv.StatsServer = newStatsServer()

	if cfg.HTTP.TLSKeyPath != "" && cfg.HTTP.TLSCertPath != "" {
		srv.CertLoader, err = NewCertReloader(cfg.HTTP.TLSCertPath, cfg.HTTP.TLSKeyPath)
		if err != nil {
			return nil, fmt.Errorf("create cert reloader: %w", err)
		}

		srv.HTTPServer.SetTLSConfig(&tls.Config{
			GetCertificate: srv.CertLoader.GetCertificateFunc(),
		})
	}

	warnIfAnonAdminAccess(ctx, be, logger)
	warnUnboundedTimeouts(cfg, logger)

	// The backend decides this on its own; surface it once at startup so the
	// operator does not learn about it from the first failed import.
	if reason := be.ImportsDisabledReason(); reason != "" {
		logger.Error("imports and mirror syncs are disabled", "reason", reason)
	}

	return srv, nil
}

// ensureKey creates an Ed25519 key at path if there is none: the host key
// the SSH server presents, or the client key the server presents to the
// ssh remotes it imports and mirrors from. An error other than a missing
// file stops the server rather than leave it without a key. The error
// starts with what.
func ensureKey(path, what string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if _, err := keygen.New(path, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	} else if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// newStatsServer returns the stats server: the Prometheus metrics on
// /metrics, and nothing else. Start binds its listener.
func newStatsServer() *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
	}
}

// warnIfAnonAdminAccess logs a loud warning if the server's effective,
// post-override settings grant unauthenticated (keyless) connections admin
// access. This checks effective runtime state via the backend, not just the
// new config-override fields, since the same risk exists whether the
// dangerous combination came from config or was set via `ssh soft settings`
// on a previous run.
func warnIfAnonAdminAccess(ctx context.Context, be *backend.Backend, logger *log.Logger) {
	if !be.AllowKeyless(ctx) || be.AnonAccess(ctx) < access.AdminAccess {
		return
	}

	logger.Warn("################################################################")
	logger.Warn("# WARNING: anonymous keyless connections have ADMIN access.    #")
	logger.Warn("# Anyone who can reach this server has full control, no auth.  #")
	logger.Warn("# This is intended for local/dev use only. Do not expose this  #")
	logger.Warn("# server to an untrusted network.                              #")
	logger.Warn("################################################################")
}

// warnUnboundedTimeouts warns about each enabled server whose max_timeout is
// 0. The idle timeouts are reset by every byte, so with no total limit a
// client that trickles bytes keeps its connection and its place under
// max_connections for as long as it likes. A config.yaml written before the
// default became non-zero still says 0, and the file is never rewritten.
func warnUnboundedTimeouts(cfg *config.Config, logger *log.Logger) {
	for _, name := range cfg.UnboundedTimeouts() {
		logger.Warn("no total time limit on connections; a non-zero value is recommended",
			"setting", name, "recommended", config.DefaultMaxTimeout)
	}
}

// ensureDefaultRepo creates the repo named by cfg.DefaultRepo if it does not
// already exist. It never fails startup: it logs invalid names and creation
// errors, then returns.
func ensureDefaultRepo(ctx context.Context, cfg *config.Config, be *backend.Backend, logger *log.Logger) {
	if cfg.DefaultRepo == "" {
		return
	}

	name := utils.SanitizeRepo(cfg.DefaultRepo)
	if err := utils.ValidateRepo(name); err != nil {
		logger.Warn("invalid default_repo, skipping", "name", cfg.DefaultRepo, "err", err)
		return
	}

	if _, err := be.Repository(ctx, name); err == nil {
		return
	} else if !errors.Is(err, proto.ErrRepoNotFound) {
		logger.Warn("failed to look up default repo", "name", name, "err", err)
		return
	}

	// The repos table requires a non-null owner, so we attribute the repo to
	// the lowest-ID admin, the same account that owns repositories created
	// anonymously. User ID 1 is not necessarily that account: it can be
	// deleted or demoted, and a database imported from an older version
	// keeps that version's user IDs. This is the server's own bootstrap, not
	// a user request, so it is not subject to admin_only_create.
	owner, err := be.DefaultAdmin(ctx)
	if err != nil {
		logger.Warn("failed to look up default repo owner, skipping", "name", name, "err", err)
		return
	}

	switch _, err := be.CreateSystemRepository(ctx, name, owner, proto.RepositoryOptions{Private: cfg.DefaultPrivate}); {
	case errors.Is(err, proto.ErrRepoExist):
		logger.Warn("default repo directory exists without a database row, skipping", "name", name)
	case err != nil:
		logger.Warn("failed to create default repo", "name", name, "err", err)
	default:
		logger.Info("created default repo", "name", name)
	}
}

// ReloadCertificates reloads the TLS certificates for the HTTP server.
func (s *Server) ReloadCertificates() error {
	if s.CertLoader == nil {
		return nil
	}
	return s.CertLoader.Reload()
}

// Start binds the listener of every enabled server and then serves them all
// until they stop.
//
// All listeners are bound before any server starts serving. If one cannot be
// bound, for example because its port is in use, Start closes the ones
// already bound and returns that error at once. Otherwise the process would
// keep running with only some of its servers up, and the failed server's
// error would surface only once all the others had stopped. It also means
// that once any of the servers accepts connections, all of them do.
//
// Likewise, if a server fails while serving, Start returns its error at once,
// without waiting for the others to stop. The caller should then shut the
// server down.
//
// Start returns nil without serving if Shutdown or Close has been called, and
// an error if no server is enabled. ctx bounds the binding of the
// listeners; Shutdown and Close stop the servers.
func (s *Server) Start(ctx context.Context) error {
	type service struct {
		name   string
		addr   string
		serve  func(net.Listener) error
		closed error // the error serve returns after a shutdown
	}
	var services []service
	if s.Config.SSH.Enabled {
		services = append(services, service{"SSH server", s.Config.SSH.ListenAddr, s.SSHServer.Serve, ssh.ErrServerClosed})
	}
	if s.Config.Git.Enabled {
		services = append(services, service{"Git daemon", s.Config.Git.ListenAddr, s.GitDaemon.Serve, daemon.ErrServerClosed})
	}
	if s.Config.HTTP.Enabled {
		services = append(services, service{"HTTP server", s.Config.HTTP.ListenAddr, s.HTTPServer.Serve, http.ErrServerClosed})
	}
	if s.Config.Stats.Enabled {
		services = append(services, service{"Stats server", s.Config.Stats.ListenAddr, s.StatsServer.Serve, http.ErrServerClosed})
	}
	if len(services) == 0 {
		// Otherwise Start would return at once and the process would exit
		// successfully without having served anything.
		return errors.New("no server is enabled; enable at least one of ssh, git, http or stats")
	}

	listeners := make([]*serveListener, 0, len(services))
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	for _, svc := range services {
		// An empty address would make net.Listen pick a random port on
		// every interface, where the servers used to fall back to their
		// library's default port. Neither is what an operator would expect
		// from a missing setting.
		if svc.addr == "" {
			closeAll()
			return fmt.Errorf("start %s: listen address is empty", svc.name)
		}
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", svc.addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("start %s: %w", svc.name, err)
		}
		listeners = append(listeners, newServeListener(l))
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeAll()
		return nil
	}
	s.listeners = listeners
	s.mu.Unlock()

	// Each server reports here when it stops. The first one to fail ends
	// Start at once, rather than once every other server has stopped too,
	// so that the process does not keep running with only some of its
	// servers up.
	errc := make(chan error, len(services))
	for i, svc := range services {
		l := listeners[i]
		go func() {
			defer l.markServing()
			s.logger.Print("Starting "+svc.name, "addr", l.Addr().String())
			err := svc.serve(l)
			if errors.Is(err, svc.closed) || errors.Is(err, net.ErrClosed) && s.isClosed() {
				// A server that was shut down before it started serving,
				// or before its own Shutdown or Close ran, reports the
				// listener closeListeners closed rather than its
				// ErrServerClosed.
				err = nil
			} else if err != nil {
				err = fmt.Errorf("%s: %w", svc.name, err)
			}
			errc <- err
		}()
	}

	s.Cron.Start()
	for range services {
		if err := <-errc; err != nil {
			s.logger.Error("server stopped", "err", err)
			return err
		}
	}
	return nil
}

// drainTimeout is how long the clones, pushes and SSH sessions in flight
// get to finish when the server stops. What still runs then is cut off, and
// the stop still succeeds. Webhook deliveries get their own time,
// backend.ShutdownDeliveryTimeout, and cancelled cron jobs jobsTimeout.
const drainTimeout = 5 * time.Second

// jobsTimeout is how long Shutdown waits at least for the cancelled cron
// jobs to end, so none uses the database once the caller closes it: until
// the caller's deadline, or this long from then on if that is later.
const jobsTimeout = 10 * time.Second

// waitAtLeast returns a context that ends at ctx's deadline or d from now,
// whichever is later, and not when ctx is cancelled before that.
func waitAtLeast(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.After(deadline) {
		deadline = dl
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// Shutdown stops the server. Imports in flight are cancelled, and the
// servers drain within ctx; what still runs when ctx is done is closed, which
// is logged but not an error. The cancelled cron jobs are waited for until
// ctx's deadline, and for at least jobsTimeout. The cancelled imports'
// cleanup and the background webhook deliveries are then waited for within
// their own budget, backend.ShutdownDeliveryTimeout, or until abort is done,
// whichever comes first; with abort already done, as for Close, they are
// not waited for. Each server's error is logged; they are returned joined. The
// servers that were never built are skipped, so it also stops a server
// NewServer failed to finish.
func (s *Server) Shutdown(ctx context.Context, abort context.Context) error {
	s.markClosed()
	logger := s.logger
	if logger == nil {
		logger = log.Default() // a Server NewServer did not build
	}
	// Stop the imports first: an SSH session running one would otherwise
	// hold the SSH server's drain until the import finished.
	if s.Backend != nil {
		s.Backend.StopImports()
	}
	// Close the listeners first, and wait until every server serves on its
	// listener, which then fails at once, or has given up on it. A server's
	// Shutdown only closes the listeners it already serves on, and the SSH
	// server's then waits for every listener it serves on to be released, so
	// one that started serving just after its Shutdown ran held it until
	// the deadline. The SSH server and the Git daemon also count a listener
	// they start serving on with a WaitGroup their Shutdown waits on, which
	// must not start counting while it is being waited on.
	for _, l := range s.closeListeners() {
		select {
		case <-l.serving:
		case <-ctx.Done():
		}
	}

	var (
		errg errgroup.Group
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		logger.Error("shutdown", "err", err)
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	stop := func(name string, shutdown func(context.Context) error, closeNow func() error) {
		errg.Go(func() error {
			err := shutdown(ctx)
			if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				// Only the drain ran out: cut off what still runs.
				if errors.Is(err, context.DeadlineExceeded) {
					// Not for Close, whose context is cancelled
					// before any drain.
					logger.Warn("stopping with connections still open", "server", name, "drain", drainTimeout)
				}
				err = closeNow()
				if errors.Is(err, daemon.ErrServerClosed) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, ssh.ErrServerClosed) {
					// The shutdown had closed it already; Close cut
					// off its connections all the same.
					err = nil
				}
			}
			if err != nil {
				fail(fmt.Errorf("%s: %w", name, err))
			}
			return nil
		})
	}
	if s.GitDaemon != nil {
		stop("Git daemon", s.GitDaemon.Shutdown, s.GitDaemon.Close)
	}
	if s.HTTPServer != nil {
		stop("HTTP server", s.HTTPServer.Shutdown, s.HTTPServer.Close)
	}
	if s.SSHServer != nil {
		stop("SSH server", s.SSHServer.Shutdown, s.SSHServer.Close)
	}
	if s.StatsServer != nil {
		stop("Stats server", s.StatsServer.Shutdown, s.StatsServer.Close)
	}
	if s.stopJobs != nil {
		// Cancel the running jobs; Shutdown also stops the scheduler
		// from starting any more.
		s.stopJobs()
	}
	if s.Cron != nil {
		errg.Go(func() error {
			jctx, cancel := waitAtLeast(ctx, jobsTimeout)
			defer cancel()
			if err := s.Cron.Shutdown(jctx); err != nil {
				fail(fmt.Errorf("wait for cron jobs: %w", err))
			}
			return nil
		})
	}
	_ = errg.Wait()

	// Always wait for the deliveries, even when a server failed to stop:
	// the queue is closed here so no new delivery starts, and the ones
	// already queued or running get their own budget, which is separate from
	// the caller's drain deadline for the HTTP and SSH servers. The stopped
	// imports are waited for first, within the same budget, since cleaning
	// up a registered one queues a repository delete event.
	// An abort that is already done, as Close's, waits for nothing; the
	// queue is still closed, so no new delivery starts.
	if s.Backend != nil && abort.Err() != nil {
		s.Backend.StopDeliveries()
	} else if s.Backend != nil {
		dctx, cancel := context.WithTimeout(abort, backend.ShutdownDeliveryTimeout)
		defer cancel()
		s.Backend.WaitForImports(dctx)
		s.Backend.WaitForDeliveries(dctx)
	}
	return errors.Join(errs...)
}

// closeServer closes a server NewServer could not finish building.
func closeServer(s *Server) { _ = s.Close() }

// Close stops the server at once: it is Shutdown with contexts that are
// already done, so nothing is drained and no delivery is waited for. Its
// callers stop a server that has not served yet.
func (s *Server) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return s.Shutdown(ctx, ctx)
}

// markClosed makes a Start that has not yet stored its listeners close them
// and return instead of serving.
func (s *Server) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// closeListeners closes the listeners Start bound, and returns them. A server
// that was shut down before it started serving on its listener does not know
// about it, so would otherwise leave it open, and the SSH server would even go
// on to serve on it. It runs before the servers' own Shutdown or Close, which
// close the listeners they serve on again; serveListener makes that a no-op.
func (s *Server) closeListeners() []*serveListener {
	s.mu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}
	return listeners
}

// serveListener is a listener Start bound. It closes the listener only once,
// and returns nil from every later Close: both closeListeners and the server
// serving on the listener close it, and the server would otherwise report the
// second Close as an error. Its serving channel is closed once the server
// first accepts on it, which every server does only after it has registered
// the listener, or once the server has returned without doing so.
type serveListener struct {
	net.Listener
	closeOnce   sync.Once
	servingOnce sync.Once
	serving     chan struct{}
}

func newServeListener(l net.Listener) *serveListener {
	return &serveListener{Listener: l, serving: make(chan struct{})}
}

func (l *serveListener) Accept() (net.Conn, error) {
	l.markServing()
	return l.Listener.Accept()
}

func (l *serveListener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.Listener.Close() })
	return err
}

func (l *serveListener) markServing() {
	l.servingOnce.Do(func() { close(l.serving) })
}

// jobScheduler is the part of *cron.Scheduler scheduleJobs uses.
type jobScheduler interface {
	AddFunc(name, spec string, fn func()) error
}

// scheduleJobs adds the server's periodic jobs, of cfg and be, to sched:
// mirror-pull on jobs.mirror_pull, which an empty value disables, and
// lfs-sweep hourly (jobs.LFSSweepSpec). The jobs log to logger, and run with
// jobCtx, which the server cancels when it stops. A tick that fires once
// jobCtx is done, between the cancellation and the scheduler's stop, starts
// nothing. A job the scheduler refuses is logged; a job that cannot be made
// is an error.
func scheduleJobs(jobCtx context.Context, cfg *config.Config, be *backend.Backend, sched jobScheduler, logger *log.Logger) error {
	add := func(name, spec string, fn func(context.Context)) {
		run := func() {
			if jobCtx.Err() != nil {
				return
			}
			fn(jobCtx)
		}
		if err := sched.AddFunc(name, spec, run); err != nil {
			logger.Warn("error adding cron job", "job", name, "err", err)
		}
	}

	if cfg.Jobs.MirrorPull == "" {
		logger.Info("cron job disabled", "job", "mirror-pull")
	} else {
		mirror, err := jobs.NewMirrorPull(cfg, be, logger)
		if err != nil {
			return fmt.Errorf("mirror-pull job: %w", err)
		}
		add("mirror-pull", cfg.Jobs.MirrorPull, mirror.Run)
	}

	sweep, err := jobs.NewLFSSweep(cfg, logger)
	if err != nil {
		return fmt.Errorf("lfs-sweep job: %w", err)
	}
	add("lfs-sweep", jobs.LFSSweepSpec, sweep.Run)
	return nil
}
