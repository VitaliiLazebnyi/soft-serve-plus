package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
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

// ErrPushNotSupported is returned to clients that try to push over git://.
var ErrPushNotSupported = errors.New("pushing over git:// is not supported, use ssh or http")

// requestTimeout is how long a client has to send its request line, like
// git-daemon's --init-timeout. Unlike the idle timeout, reading does not
// extend it.
const requestTimeout = 10 * time.Second

// GitDaemon represents a Git daemon.
type GitDaemon struct {
	ctx            context.Context
	addr           string
	requestTimeout time.Duration
	finished       chan struct{}
	conns          connections
	cfg            *config.Config
	be             *backend.Backend
	wg             sync.WaitGroup
	once           sync.Once
	logger         *log.Logger
	done           atomic.Bool // indicates if the server has been closed
	listeners      []net.Listener
	liMu           sync.Mutex

	// accessTimeout bounds the access checks of each request.
	accessTimeout time.Duration
}

// NewGitDaemon returns a new Git daemon.
func NewGitDaemon(ctx context.Context) (*GitDaemon, error) {
	cfg := config.FromContext(ctx)
	addr := cfg.Git.ListenAddr
	d := &GitDaemon{
		ctx:            ctx,
		addr:           addr,
		requestTimeout: requestTimeout,
		finished:       make(chan struct{}, 1),
		cfg:            cfg,
		be:             backend.FromContext(ctx),
		conns:          connections{m: make(map[net.Conn]struct{})},
		logger:         log.FromContext(ctx).WithPrefix("gitdaemon"),

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

// Serve accepts connections on the listener and serves Git requests.
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

		// Count the connection here rather than in its goroutine, so that
		// the limit holds for a burst of connections and a connection
		// accepted while the daemon closes is closed with the rest.
		ctx, cancel := d.requestContext()
		c := d.newServerConn(conn, cancel)
		if err := d.conns.Add(c, d.cfg.Git.MaxConnections); err != nil {
			if errors.Is(err, git.ErrMaxConnections) {
				// Close connection if there are too many open connections.
				d.logger.Debugf("git: max connections reached, closing %s", conn.RemoteAddr())
				d.fatal(c, err)
			} else {
				c.Close() //nolint: errcheck
			}
			continue
		}

		d.wg.Add(1)
		go func() {
			d.handleClient(ctx, c)
			d.wg.Done()
		}()
	}
}

// newServerConn wraps conn with the configured timeouts; cancel is called
// when the connection fails or is closed.
func (d *GitDaemon) newServerConn(conn net.Conn, cancel context.CancelFunc) *serverConn {
	c := &serverConn{
		Conn:          conn,
		initDeadline:  time.Now().Add(d.requestTimeout),
		idleTimeout:   time.Duration(d.cfg.Git.IdleTimeout) * time.Second,
		closeCanceler: cancel,
	}
	if d.cfg.Git.MaxTimeout > 0 {
		dur := time.Duration(d.cfg.Git.MaxTimeout) * time.Second
		c.maxDeadline = time.Now().Add(dur)
	}
	return c
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

// handleClient handles a git protocol client. c must already be in d.conns;
// ctx is canceled when c fails or is closed.
func (d *GitDaemon) handleClient(ctx context.Context, c *serverConn) {
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
		d.fatal(c, git.ErrTimeout)
	})
	s := pktline.NewScanner(c)
	scanned := s.Scan()
	if !stopRead() {
		// The server's context ended and the client has been answered.
		return
	}
	// The read is over: from here on reads and writes only have to keep
	// the connection from going idle.
	c.initDeadline = time.Time{}
	if !scanned {
		err := s.Err()
		var nerr net.Error
		switch {
		case err == nil:
			// The client hung up without sending a request.
		case errors.As(err, &nerr) && nerr.Timeout():
			d.fatal(c, git.ErrTimeout)
		case errors.Is(err, pktline.ErrInvalidPktLen), isErrorLine(err):
			d.logger.Debugf("git: invalid request from %s: %v", c.RemoteAddr(), err)
			d.fatal(c, git.ErrInvalidRequest)
		default:
			// The connection was reset, cut short, or closed by the server:
			// there is no one to answer.
			d.logger.Debugf("git: error reading request from %s: %v", c.RemoteAddr(), err)
		}
		return
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
	case git.ReceivePackService:
		d.fatal(c, ErrPushNotSupported)
		return
	default:
		d.fatal(c, git.ErrInvalidRequest)
		return
	}

	opts := bytes.SplitN(split[1], []byte{0}, 3)
	if len(opts) < 2 {
		d.fatal(c, git.ErrInvalidRequest)
		return
	}

	host := strings.TrimPrefix(string(opts[1]), "host=")
	var version string
	if len(opts) > 2 {
		version = protocolVersion(opts[2])
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

	// Pass the requested protocol version down to git.
	if version != "" {
		envs = append(envs, "GIT_PROTOCOL=version="+version)
	}

	envs = append(envs, d.cfg.Environ()...)

	// git's stderr goes to the log, as git-daemon does: the daemon has
	// no side channel for it, and written to the connection it would
	// corrupt the protocol stream and could show server details to
	// anonymous clients.
	stderr := d.logger.With("addr", c.RemoteAddr(), "service", service, "repo", name).
		StandardLog(log.StandardLogOptions{ForceLevel: log.WarnLevel}).Writer()
	cmd := git.ServiceCommand{
		Stdin:  c,
		Stdout: c,
		Stderr: stderr,
		Env:    envs,
		Dir:    filepath.Join(reposDir, repo),
	}
	if ka := keepAlive(c.idleTimeout); ka > 0 {
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
		// Before that, the client gets a fixed message rather than
		// the error itself.
		if errors.Is(err, git.ErrInvalidRepo) {
			d.fatal(c, git.ErrInvalidRepo)
		} else {
			d.fatal(c, git.ErrSystemMalfunction)
		}
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

// Close closes the listeners and all open connections.
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

// isErrorLine reports whether err is an ERR packet the client sent.
func isErrorLine(err error) bool {
	var el *pktline.ErrorLine
	return errors.As(err, &el)
}

// protocolVersion returns the protocol version the client asked for in the
// extra parameters of its request, or "" if it asked for none. Like git, it
// takes the highest "version=N" and ignores every other parameter, so nothing
// else the client sends reaches GIT_PROTOCOL.
func protocolVersion(params []byte) string {
	version := -1
	for p := range bytes.SplitSeq(params, []byte{0}) {
		v, ok := bytes.CutPrefix(p, []byte("version="))
		if !ok || len(v) == 0 || len(v) > 3 {
			continue
		}
		n, err := strconv.Atoi(string(v))
		if err != nil || n < 0 {
			continue
		}
		version = max(version, n)
	}
	if version < 0 {
		return ""
	}
	return strconv.Itoa(version)
}
