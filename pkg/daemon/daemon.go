package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/safego"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	uploadPackGitCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "daemon",
		Name:      "git_upload_pack_total",
		Help:      "The total number of git-upload-pack requests",
	}, []string{"repo"})

	uploadArchiveGitCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "daemon",
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

	// refuseLinger bounds how long a refusal waits for the client to hang
	// up; see refuse.
	refuseLinger time.Duration

	// refusing holds a token for each connection refuseAsync refuses.
	refusing chan struct{}

	testHooks testHooks
}

// testHooks are called by the daemon while it serves, for tests to inject
// delays and faults. They are nil outside tests.
type testHooks struct {
	// beforeAuthorize runs before the access checks of a request.
	beforeAuthorize func()
	// now, if set, is the clock refuse reads the time from, so that a
	// test can have a refusal come after a deadline without waiting
	// for it to pass.
	now func() time.Time
}

// now returns the time refuse counts its wait from.
func (d *GitDaemon) now() time.Time {
	if d.testHooks.now != nil {
		return d.testHooks.now()
	}
	return time.Now()
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
		conns:          newConnections(),
		logger:         log.FromContext(ctx).WithPrefix("gitdaemon"),

		accessTimeout: defaultAccessTimeout,
		refuseLinger:  refuseLinger,
		refusing:      make(chan struct{}, maxRefusing),
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
		if err := d.conns.Add(c, d.cfg.Git.MaxConnections, d.cfg.Git.MaxConnectionsPerIP); err != nil {
			if errors.Is(err, git.ErrMaxConnections) {
				// Close connection if there are too many open connections,
				// in all or from its address.
				d.logger.Debugf("git: %v, closing %s", err, conn.RemoteAddr())
				d.refuseAsync(c, git.ErrMaxConnections)
			} else {
				c.Close() //nolint: errcheck
			}
			continue
		}

		// A panic while the connection is handled is logged and ends
		// only this connection: handleClient's deferred close still
		// gives its slot back, and the goroutine still leaves d.wg.
		d.wg.Go(func() {
			safego.Run(d.logger, func() { d.handleClient(ctx, c) }) //nolint: errcheck // Run logs a panic.
		})
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

// fatal sends err to the client and closes the connection at once. It is
// for shutdown, and for the accept loop when it has too many refusals in
// progress, which must not wait on a client; the answer to a client's
// request goes through refuse.
func (d *GitDaemon) fatal(c net.Conn, err error) {
	git.WritePktlineErr(c, err) //nolint: errcheck
	if err := c.Close(); err != nil {
		d.logger.Debugf("git: error closing connection: %v", err)
	}
}

// refuseLinger bounds how long refuse waits for a refused client to hang
// up, and refuseDrain how much of what it sends refuse reads meanwhile.
// maxRefusing bounds how many connections refused for the connection limit
// do so at once.
const (
	refuseLinger = time.Second
	refuseDrain  = 64 << 10
	maxRefusing  = 16
)

// refuseAsync refuses c, a connection the accept loop turns away, on a
// goroutine of its own, so that the accept loop does not wait on the client.
// Such a connection is not counted against max_connections, so at most
// maxRefusing of them are refused this way at once, which bounds the file
// descriptors they hold; past that, c gets the error and is closed at once,
// and the client may lose the error to a reset as it did before.
func (d *GitDaemon) refuseAsync(c *serverConn, err error) {
	select {
	case d.refusing <- struct{}{}:
	default:
		d.fatal(c, err)
		return
	}
	// Serve, the only caller, is counted in d.wg, so Shutdown cannot be
	// past its Wait.
	d.wg.Go(func() {
		defer func() { <-d.refusing }()
		safego.Run(d.logger, func() { //nolint: errcheck // Run logs a panic.
			// refuse closes c; this closes it if refuse panics.
			defer c.Close() //nolint: errcheck
			d.refuse(c, err, time.Time{})
		})
	})
}

// refuse sends err to the client and closes the connection without losing
// the error.
//
// A client may send more than the request line before it reads the answer:
// git archive sends its arguments straight after the request. Closing a
// socket that still receives input makes the kernel answer with a reset, and
// a client that gets the reset before it has read the error loses the error
// with it: a write fails, or the read reports a reset connection. So refuse
// shuts the write side first, which delivers the error followed by
// end-of-file, then reads and drops what the client still sends until it
// hangs up, and only then closes.
//
// The wait ends refuseLinger after the answer, at until unless it is zero,
// and at the connection's maximum deadline, whichever comes first, and reads
// at most refuseDrain bytes. It also ends when the server's context does or
// the daemon shuts down, so a graceful Shutdown does not wait on refused
// clients. A connection counted against max_connections stays counted
// meanwhile, as it is still open. A refusal of the request passes the request deadline
// as until, so a client holds its connection unanswered no longer than it
// could already by sending the request line late, however long the access
// checks took.
func (d *GitDaemon) refuse(c *serverConn, err error, until time.Time) {
	git.WritePktlineErr(c, err) //nolint: errcheck
	now := d.now()
	deadline := earliest(now.Add(d.refuseLinger), until, c.maxDeadline)
	if deadline.After(now) {
		if cw := closeWriter(c.Conn); cw != nil && cw.CloseWrite() == nil {
			// Read the wrapped connection: serverConn.Read would move
			// the deadline.
			_ = c.Conn.SetReadDeadline(deadline)
			stop := d.onClosing(func() { _ = c.Conn.SetReadDeadline(aLongTimeAgo) })
			_, _ = io.CopyN(io.Discard, c.Conn, refuseDrain)
			stop()
		}
	}
	if err := c.Close(); err != nil {
		d.logger.Debugf("git: error closing connection: %v", err)
	}
}

// aLongTimeAgo is a deadline that has passed, which ends a blocked read.
var aLongTimeAgo = time.Unix(1, 0)

// onClosing calls f once, on another goroutine, when the server's context
// ends or the daemon shuts down or closes, unless stop is called first. stop
// returns once f will not be called or has returned.
func (d *GitDaemon) onClosing(f func()) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-d.ctx.Done():
		case <-d.finished:
		case <-quit:
			return
		}
		f()
	}()
	return func() {
		close(quit)
		<-done
	}
}

// earliest returns the earliest of t and the non-zero times in ts.
func earliest(t time.Time, ts ...time.Time) time.Time {
	for _, u := range ts {
		if !u.IsZero() && u.Before(t) {
			t = u
		}
	}
	return t
}

// closeWriter returns the connection under c that can shut its write side
// alone, such as a *net.TCPConn, looking through wrappers that expose the
// connection they wrap with NetConn. It returns nil if there is none.
func closeWriter(c net.Conn) interface{ CloseWrite() error } {
	for c != nil {
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			return cw
		}
		w, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = w.NetConn()
	}
	return nil
}

// requestContext returns the context a request is served with. It derives
// from the server's context, so the request carries the server's config,
// backend and logger and ends when the server's context does.
func (d *GitDaemon) requestContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(d.ctx)
}

// readable reports whether the anonymous daemon may serve the repository
// name. Its database lookups get at most accessTimeout. The error is set
// when a lookup failed, one that ran out of time included; the request is
// then refused as a server fault.
func (d *GitDaemon) readable(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, d.accessTimeout)
	defer cancel()
	allow, err := d.be.AllowKeylessSetting(ctx)
	if err != nil || !allow {
		return false, err
	}
	r, _, err := d.be.Readable(ctx, name, nil)
	return r != nil, err
}

// handleClient handles a git protocol client. c must already be in d.conns;
// ctx is canceled when c fails or is closed.
func (d *GitDaemon) handleClient(ctx context.Context, c *serverConn) {
	defer func() {
		d.conns.Close(c) //nolint: errcheck
	}()

	line, until, ok := d.readRequest(c)
	if !ok {
		return
	}
	req, err := parseRequest(line)
	if err != nil {
		d.refuse(c, err, until)
		return
	}
	if req.version != "" {
		d.logger.Debugf("git: protocol version %s", req.version)
	}
	if d.testHooks.beforeAuthorize != nil {
		d.testHooks.beforeAuthorize()
	}
	if err := d.authorize(ctx, req); err != nil {
		d.refuse(c, err, until)
		return
	}
	d.serve(ctx, c, req)
}

// readRequest reads the request line from c. It answers the client itself
// when the read fails, and then reports !ok. Otherwise it returns the line
// and the request deadline, which bounds how long a refusal of the request
// may wait on the client.
func (d *GitDaemon) readRequest(c *serverConn) (line []byte, until time.Time, ok bool) {
	// Read the request line on this goroutine. The connection's deadlines
	// bound the read, and closing the connection on shutdown unblocks it, so
	// there is nothing to wait on beside it. If the server's context ends
	// first, the client gets the timeout answer and the connection is closed,
	// which ends the read too.
	stopRead := context.AfterFunc(d.ctx, func() {
		d.logger.Debugf("git: connection context error: %v", d.ctx.Err())
		d.fatal(c, git.ErrTimeout)
	})
	// A panic in the read must not leave the AfterFunc registered on the
	// server's context; stopping it again below is harmless.
	defer stopRead()
	line, err := git.ReadPkt(c)
	if !stopRead() {
		// The server's context ended and the client has been answered.
		return nil, time.Time{}, false
	}
	// The read is over: from here on reads and writes only have to keep
	// the connection from going idle. A refusal of the request may still
	// wait on the client until the request deadline.
	until = c.initDeadline
	c.initDeadline = time.Time{}
	if err == nil {
		return line, until, true
	}
	var nerr net.Error
	switch {
	case errors.Is(err, io.EOF):
		// The client hung up without sending a request.
	case errors.As(err, &nerr) && nerr.Timeout():
		d.refuse(c, git.ErrTimeout, until)
	case errors.Is(err, git.ErrInvalidPktLen), isErrorLine(err):
		d.logger.Debugf("git: invalid request from %s: %v", c.RemoteAddr(), err)
		d.refuse(c, git.ErrInvalidRequest, until)
	default:
		// The connection was reset, cut short, or closed by the server:
		// there is no one to answer.
		d.logger.Debugf("git: error reading request from %s: %v", c.RemoteAddr(), err)
	}
	return nil, time.Time{}, false
}

// gitRequest is a parsed git:// request line.
type gitRequest struct {
	service git.Service
	// name is the repository, sanitized.
	name string
	// host is the host the client named, as it sent it.
	host string
	// version is the protocol version the client asked for, or "".
	version string
}

// counter returns the metric that counts the served requests of the
// request's service.
func (r gitRequest) counter() *prometheus.CounterVec {
	if r.service == git.UploadArchiveService {
		return uploadArchiveGitCounter
	}
	return uploadPackGitCounter
}

// parseRequest parses a request line, "<service> <path>\x00host=<host>\x00"
// optionally followed by "\x00<extra parameters>". For a request the daemon
// does not serve, it returns the error to answer the client with.
func parseRequest(line []byte) (gitRequest, error) {
	split := bytes.SplitN(line, []byte{' '}, 2)
	if len(split) != 2 {
		return gitRequest{}, git.ErrInvalidRequest
	}

	service := git.Service(split[0])
	switch service {
	case git.UploadPackService, git.UploadArchiveService:
	case git.ReceivePackService:
		return gitRequest{}, ErrPushNotSupported
	default:
		return gitRequest{}, git.ErrInvalidRequest
	}

	opts := bytes.SplitN(split[1], []byte{0}, 3)
	if len(opts) < 2 {
		return gitRequest{}, git.ErrInvalidRequest
	}

	req := gitRequest{
		service: service,
		name:    utils.SanitizeRepo(string(opts[0])),
		host:    strings.TrimPrefix(string(opts[1]), "host="),
	}
	if len(opts) > 2 {
		req.version = protocolVersion(opts[2])
	}
	return req, nil
}

// reposDir returns the directory the repositories are in.
func (d *GitDaemon) reposDir() string {
	return hooks.ReposPath(d.cfg)
}

// repoDir returns the directory of the request's repository.
func (d *GitDaemon) repoDir(req gitRequest) string {
	return hooks.RepoPath(d.cfg, req.name)
}

// authorize returns nil if the daemon may serve req, and otherwise the error
// to answer the client with.
func (d *GitDaemon) authorize(ctx context.Context, req gitRequest) error {
	if err := git.EnsureWithin(d.reposDir(), req.name+".git"); err != nil {
		d.logger.Debugf("git: error ensuring repo path: %v", err)
		return git.ErrNotAuthed
	}

	// The daemon is always anonymous, so a repository it cannot read
	// gets the anonymous answer whether it is private or missing, and
	// so does every repository when keyless access is off. A lookup that
	// failed is a server fault, not a missing repository.
	ok, err := d.readable(ctx, req.name)
	if err != nil {
		if ctx.Err() != nil {
			d.logger.Debug("git: repository lookup canceled", "repo", req.name, "err", err)
		} else {
			d.logger.Error("git: failed to look up repository", "repo", req.name, "err", err)
		}
		return git.ErrSystemMalfunction
	}
	if !ok {
		return git.ErrNotAuthed
	}
	return nil
}

// serve runs git for an authorized request, with c as its input and output.
func (d *GitDaemon) serve(ctx context.Context, c *serverConn, req gitRequest) {
	service, name := req.service, req.name
	req.counter().WithLabelValues(name).Inc()

	start := time.Now()
	d.logger.Info("git: accepted request", "remote", c.RemoteAddr(), "service", service, "repo", name)
	defer func() {
		elapsed := time.Since(start)
		requestSeconds.WithLabelValues(service.String()).Observe(elapsed.Seconds())
		d.logger.Debug("git: finished request", "remote", c.RemoteAddr(), "service", service, "repo", name, "duration", elapsed)
	}()

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
		Env:    d.environ(req),
		Dir:    d.repoDir(req),
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
			// corrupt it. Closing the connection, which
			// handleClient does, is all that is left.
			return
		}
		// Before that, the client gets a fixed message rather than
		// the error itself.
		if errors.Is(err, git.ErrInvalidRepo) {
			d.refuse(c, git.ErrInvalidRepo, time.Time{})
		} else {
			d.refuse(c, git.ErrSystemMalfunction, time.Time{})
		}
	}
}

// environ returns the environment git runs with for req.
//
// The daemon serves only upload-pack and upload-archive, which run no hooks,
// so git gets no hook environment and none of the server config: only the
// requested protocol version, as GIT_PROTOCOL.
func (d *GitDaemon) environ(req gitRequest) []string {
	if req.version == "" {
		return nil
	}
	return hooks.ProtocolEnv("version=" + req.version)
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
	safego.Go(d.logger, func() {
		defer close(finished)
		d.wg.Wait()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-finished:
		return err
	}
}

// isErrorLine reports whether err is an ERR packet the client sent.
func isErrorLine(err error) bool {
	var el *git.PktlineError
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
