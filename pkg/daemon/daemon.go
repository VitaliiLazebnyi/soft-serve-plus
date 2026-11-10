package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	uploadPackGitCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "git_upload_pack_total",
		Help:      "The total number of git-upload-pack requests",
	}, []string{"repo"})

	uploadArchiveGitCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "git",
		Name:      "git_upload_archive_total",
		Help:      "The total number of git-upload-archive requests",
	}, []string{"repo"})

	// requestSeconds is labelled with the service only: the repository name
	// comes from the client and would make the label set unbounded.
	requestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "soft_serve",
		Subsystem: "daemon",
		Name:      "request_duration_seconds",
		Help:      "The time spent serving accepted git:// requests",
		Buckets:   []float64{.01, .05, .1, .5, 1, 5, 10, 30, 60, 300, 900},
	}, []string{"service"})
)

// defaultAccessTimeout bounds the database lookups that decide whether the
// daemon may serve a request, so that a slow or stuck database refuses the
// client instead of holding its connection until the idle or max timeout.
const defaultAccessTimeout = 10 * time.Second

// ErrServerClosed indicates that the server has been closed.
var ErrServerClosed = fmt.Errorf("git: %w", net.ErrClosed)

// GitDaemon represents a Git daemon.
type GitDaemon struct {
	ctx       context.Context
	addr      string
	finished  chan struct{}
	conns     connections
	cfg       *config.Config
	be        *backend.Backend
	wg        sync.WaitGroup
	once      sync.Once
	logger    *log.Logger
	done      atomic.Bool // indicates if the server has been closed
	listeners []net.Listener
	liMu      sync.Mutex

	// accessTimeout bounds the access checks of each request.
	accessTimeout time.Duration
}

// NewGitDaemon returns a new Git daemon.
func NewGitDaemon(ctx context.Context) (*GitDaemon, error) {
	cfg := config.FromContext(ctx)
	addr := cfg.Git.ListenAddr
	d := &GitDaemon{
		ctx:      ctx,
		addr:     addr,
		finished: make(chan struct{}, 1),
		cfg:      cfg,
		be:       backend.FromContext(ctx),
		conns:    connections{m: make(map[net.Conn]struct{})},
		logger:   log.FromContext(ctx).WithPrefix("gitdaemon"),

		accessTimeout: defaultAccessTimeout,
	}
	return d, nil
}

// ListenAndServe starts the Git TCP daemon.
func (d *GitDaemon) ListenAndServe() error {
	if d.done.Load() {
		return ErrServerClosed
	}
	var cfg net.ListenConfig
	listener, err := cfg.Listen(d.ctx, "tcp", d.addr)
	if err != nil {
		return err
	}
	return d.Serve(listener)
}

// Serve listens on the TCP network address and serves Git requests.
func (d *GitDaemon) Serve(listener net.Listener) error {
	// Register under liMu, which closeListener holds while it marks the
	// daemon done, so that Serve either sees it done or is counted before
	// Shutdown starts waiting.
	d.liMu.Lock()
	if d.done.Load() {
		d.liMu.Unlock()
		return ErrServerClosed
	}
	d.wg.Add(1)
	d.listeners = append(d.listeners, listener)
	d.liMu.Unlock()
	defer d.wg.Done()

	var tempDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-d.finished:
				return ErrServerClosed
			default:
				d.logger.Debugf("git: error accepting connection: %v", err)
			}
			// Accept errors such as EMFILE are the case Temporary is still
			// meant for; net/http's accept loop backs off on it the same way.
			var ne net.Error
			//lint:ignore SA1019 accept errors are what Temporary is still meant for.
			if errors.As(err, &ne) && ne.Temporary() { //nolint:staticcheck // see above
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				tempDelay = min(tempDelay, time.Second)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}

		// Close connection if there are too many open connections.
		if d.conns.Size()+1 >= d.cfg.Git.MaxConnections {
			d.logger.Debugf("git: max connections reached, closing %s", conn.RemoteAddr())
			d.fatal(conn, git.ErrMaxConnections)
			continue
		}

		d.wg.Add(1)
		go func() {
			d.handleClient(conn)
			d.wg.Done()
		}()
	}
}

func (d *GitDaemon) fatal(c net.Conn, err error) {
	git.WritePktlineErr(c, err) //nolint: errcheck
	if err := c.Close(); err != nil {
		d.logger.Debugf("git: error closing connection: %v", err)
	}
}

// requestContext returns the context a request is served with. It derives
// from the server's context, so the request carries the server's config,
// backend and logger and ends when the server's context does.
func (d *GitDaemon) requestContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(d.ctx)
}

// readable reports whether the anonymous daemon may serve the repository
// name. Its database lookups get at most accessTimeout, and a lookup that
// runs out of time refuses the request.
func (d *GitDaemon) readable(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, d.accessTimeout)
	defer cancel()
	if !d.be.AllowKeyless(ctx) {
		return false
	}
	r, _ := d.be.Readable(ctx, name, nil)
	return r != nil
}

// handleClient handles a git protocol client.
func (d *GitDaemon) handleClient(conn net.Conn) {
	ctx, cancel := d.requestContext()
	defer cancel()
	idleTimeout := time.Duration(d.cfg.Git.IdleTimeout) * time.Second
	c := &serverConn{
		Conn:          conn,
		idleTimeout:   idleTimeout,
		closeCanceler: cancel,
	}
	if d.cfg.Git.MaxTimeout > 0 {
		dur := time.Duration(d.cfg.Git.MaxTimeout) * time.Second
		c.maxDeadline = time.Now().Add(dur)
	}
	d.conns.Add(c)
	defer func() {
		d.conns.Close(c) //nolint: errcheck
	}()

	// Read the request line on this goroutine. The connection's deadlines
	// bound the read, and closing the connection on shutdown unblocks it, so
	// there is nothing to wait on beside it. If the server's context ends
	// first, the client gets the timeout answer and the connection is closed,
	// which ends the read too.
	stopRead := context.AfterFunc(d.ctx, func() {
		d.logger.Debugf("git: connection context error: %v", d.ctx.Err())
		d.fatal(conn, git.ErrTimeout)
	})
	s := pktline.NewScanner(c)
	scanned := s.Scan()
	if !stopRead() {
		// The server's context ended and the client has been answered.
		return
	}
	if !scanned {
		if err := s.Err(); err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				d.fatal(c, git.ErrTimeout)
				return
			}
			d.logger.Debugf("git: error scanning pktline: %v", err)
			d.fatal(c, git.ErrSystemMalfunction)
			return
		}
	}

	line := s.Bytes()
	split := bytes.SplitN(line, []byte{' '}, 2)
	if len(split) != 2 {
		d.fatal(c, git.ErrInvalidRequest)
		return
	}

	var counter *prometheus.CounterVec
	service := git.Service(split[0])
	switch service {
	case git.UploadPackService:
		counter = uploadPackGitCounter
	case git.UploadArchiveService:
		counter = uploadArchiveGitCounter
	default:
		d.fatal(c, git.ErrInvalidRequest)
		return
	}

	opts := bytes.SplitN(split[1], []byte{0}, 3)
	if len(opts) < 2 {
		d.fatal(c, git.ErrInvalidRequest) //nolint: errcheck
		return
	}

	host := strings.TrimPrefix(string(opts[1]), "host=")
	extraParams := map[string]string{}

	if len(opts) > 2 {
		buf := bytes.TrimPrefix(opts[2], []byte{0})
		for _, o := range bytes.Split(buf, []byte{0}) {
			opt := string(o)
			if opt == "" {
				continue
			}

			kv := strings.SplitN(opt, "=", 2)
			if len(kv) != 2 {
				d.logger.Errorf("git: invalid option %q", opt)
				continue
			}

			extraParams[kv[0]] = kv[1]
		}

		version := extraParams["version"]
		if version != "" {
			d.logger.Debugf("git: protocol version %s", version)
		}
	}

	name := utils.SanitizeRepo(string(opts[0]))

	// git bare repositories should end in ".git"
	// https://git-scm.com/docs/gitrepository-layout
	repo := name + ".git"
	reposDir := filepath.Join(d.cfg.DataPath, "repos")
	if err := git.EnsureWithin(reposDir, repo); err != nil {
		d.logger.Debugf("git: error ensuring repo path: %v", err)
		d.fatal(c, git.ErrNotAuthed)
		return
	}

	// The daemon is always anonymous, so a repository it cannot read
	// gets the anonymous answer whether it is private or missing, and
	// so does every repository when keyless access is off.
	if !d.readable(ctx, name) {
		d.fatal(c, git.ErrNotAuthed)
		return
	}

	start := time.Now()
	d.logger.Info("git: accepted request", "remote", c.RemoteAddr(), "service", service, "repo", name)
	defer func() {
		elapsed := time.Since(start)
		requestSeconds.WithLabelValues(service.String()).Observe(elapsed.Seconds())
		d.logger.Debug("git: finished request", "remote", c.RemoteAddr(), "service", service, "repo", name, "duration", elapsed)
	}()

	// Environment variables to pass down to git hooks.
	envs := []string{
		"SOFT_SERVE_REPO_NAME=" + name,
		"SOFT_SERVE_REPO_PATH=" + filepath.Join(reposDir, repo),
		"SOFT_SERVE_HOST=" + host,
		"SOFT_SERVE_LOG_PATH=" + filepath.Join(d.cfg.DataPath, "log", "hooks.log"),
	}

	// Add git protocol environment variable.
	if len(extraParams) > 0 {
		var gitProto string
		for k, v := range extraParams {
			if len(gitProto) > 0 {
				gitProto += ":"
			}
			gitProto += k + "=" + v
		}
		envs = append(envs, "GIT_PROTOCOL="+gitProto)
	}

	envs = append(envs, d.cfg.Environ()...)

	cmd := git.ServiceCommand{
		Stdin:  c,
		Stdout: c,
		Stderr: c,
		Env:    envs,
		Dir:    filepath.Join(reposDir, repo),
	}
	if ka := keepAlive(idleTimeout); ka > 0 {
		// Neither side talks while pack-objects prepares the pack
		// for a quiet fetch. Have upload-pack send keepalive
		// packets often enough that the connection does not reach
		// the idle timeout meanwhile.
		cmd.Config = append(cmd.Config, fmt.Sprintf("uploadpack.keepAlive=%d", ka))
	}

	if err := service.Handler(ctx, cmd); err != nil {
		d.logger.Debugf("git: error handling request: %v", err)
		if c.Wrote() {
			// The response has started, so an ERR packet would
			// corrupt it. Closing the connection, which the
			// deferred close does, is all that is left.
			return
		}
		d.fatal(c, err)
		return
	}

	counter.WithLabelValues(name)
}

// keepAlive returns the uploadpack.keepAlive interval, in seconds, for a
// connection closed after idle without traffic: half of it, and at least one
// second, git's smallest interval. It returns 0, leaving git's default, when
// there is no idle timeout.
func keepAlive(idle time.Duration) int {
	if idle <= 0 {
		return 0
	}
	return max(int(idle/time.Second)/2, 1)
}

// Close closes the underlying listener.
func (d *GitDaemon) Close() error {
	err := d.closeListener()
	d.conns.CloseAll() //nolint: errcheck
	return err
}

// closeListener closes the listener and the finished channel.
func (d *GitDaemon) closeListener() error {
	if d.done.Load() {
		return ErrServerClosed
	}
	var err error
	d.liMu.Lock()
	defer d.liMu.Unlock()
	// Mark the daemon done before closing the listeners, so that a Serve
	// whose Accept fails because of it reports ErrServerClosed.
	d.once.Do(func() {
		d.done.Store(true)
		close(d.finished)
	})
	for _, l := range d.listeners {
		if cerr := l.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close listener %s: %w", l.Addr(), cerr))
		}
	}
	d.listeners = d.listeners[:0]
	return err
}

// Shutdown gracefully shuts down the daemon.
func (d *GitDaemon) Shutdown(ctx context.Context) error {
	if d.done.Load() {
		return ErrServerClosed
	}

	err := d.closeListener()
	finished := make(chan struct{}, 1)
	go func() {
		defer close(finished)
		d.wg.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-finished:
		return err
	}
}
