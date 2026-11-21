package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	_ "modernc.org/sqlite" // sqlite driver
)

var (
	testDaemon  *GitDaemon
	testBackend *backend.Backend
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests starts the daemon the tests talk to, runs the tests and tears the
// daemon down again. It is separate from TestMain so that its deferred
// cleanup runs before os.Exit.
func runTests(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "soft-serve-test")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck
	ctx := context.TODO()
	cfg := config.DefaultConfig()
	cfg.DataPath = tmp
	// Each request in these tests closes its connection, but the daemon notices
	// asynchronously; leave headroom so back-to-back requests are not refused
	// with "too many connections" on slow (-race) runners.
	cfg.Git.MaxConnections = 10
	cfg.Git.MaxTimeout = 100
	// Long enough that no client, however slowed by a loaded machine, is
	// timed out between its steps. A test that waits for the idle timeout
	// serves its own daemon (startIdleDaemon).
	cfg.Git.IdleTimeout = 600
	cfg.Git.ListenAddr = fmt.Sprintf("127.0.0.1:%d", test.RandomPort()) // RandomPort checks only loopback
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource, db.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer dbx.Close() //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx, cfg, nil); err != nil {
		log.Fatal(err)
	}
	datastore := database.New()
	ctx = store.WithContext(ctx, datastore)
	be := backend.New(ctx, cfg, dbx, datastore)
	testBackend = be
	ctx = backend.WithContext(ctx, be)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		log.Fatal(err)
	}
	patient(d)
	testDaemon = d
	defer d.Close() //nolint:errcheck
	// Bind before running the tests so the first dial cannot race the
	// listener and be refused.
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", cfg.Git.ListenAddr)
	if err != nil {
		log.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	return m.Run()
}

// dialDaemon connects to the test daemon. The connection is closed when the
// test ends, and reads give up after a deadline so a daemon that never
// answers fails the test instead of hanging it.
func dialDaemon(t *testing.T) net.Conn {
	t.Helper()
	return dialDaemonAt(t, testDaemon.addr)
}

// dialDaemonAt is dialDaemon to the daemon at addr.
func dialDaemonAt(t *testing.T, addr string) net.Conn {
	t.Helper()
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetReadDeadline(time.Now().Add(idleWait)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	return c
}

// idleWait is how long a test waits for a daemon to answer or close a
// connection, including one it closes once the idle timeout of
// startIdleDaemon has passed.
const idleWait = time.Minute

// patience is the request and access timeout of the tests' daemons: far
// past any wait of the tests, so that a client or a database lookup slowed
// or paused for however long a test waits does not run out of it. A test
// of one of these timeouts sets its own.
const patience = time.Hour

// patient gives d the request and access timeouts patience.
func patient(d *GitDaemon) {
	d.requestTimeout = patience
	d.accessTimeout = patience
}

// startIdleDaemon serves a daemon like the test daemon but with an idle
// timeout of one second, for a test that waits for the timeout to pass, and
// returns its address. A client of it cannot be sure to finish a step within
// the timeout, so only a test that waits for it uses this daemon.
func startIdleDaemon(t *testing.T) string {
	t.Helper()
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 1
	ctx := backend.WithContext(config.WithContext(context.Background(), &cfg), testBackend)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	patient(d)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })
	return l.Addr().String()
}

// readPktline reads one pkt-line. An ERR packet is returned as a
// *git.PktlineError carrying the daemon's message.
func readPktline(c net.Conn) (string, error) {
	line, err := git.ReadPkt(c)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(line)), nil
}

// wantErrLine fails the test unless err is an ERR packet carrying want.
func wantErrLine(t *testing.T, what string, err error, want error) {
	t.Helper()
	var errLine *git.PktlineError
	if !errors.As(err, &errLine) {
		t.Errorf("%s: expected ERR %q, got %v", what, want, err)
		return
	}
	if errLine.Text != want.Error() {
		t.Errorf("%s: expected ERR %q, got ERR %q", what, want, errLine.Text)
	}
}

// TestIdleTimeout verifies a client that connects and never sends a request
// is told it timed out once the idle timeout passes.
func TestIdleTimeout(t *testing.T) {
	c := dialDaemonAt(t, startIdleDaemon(t))
	line, err := readPktline(c)
	if err == nil {
		t.Fatalf("expected a timeout, got line %q", line)
	}
	wantErrLine(t, "idle connection", err, git.ErrTimeout)
}

// TestBadRequests verifies the daemon answers malformed requests and pushes
// with a reply that says what is wrong.
func TestBadRequests(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		// Only the bad length prefix is sent: bytes the daemon never reads
		// would make Windows reset the connection when the daemon closes it,
		// discarding the reply before the client reads it.
		{"invalid pkt-len", []byte("zzzz"), git.ErrInvalidRequest},
		{"client error packet", []byte(pkt("ERR boom\n")), git.ErrInvalidRequest},
		{"unknown service", []byte(pkt("git-frobnicate /repo.git\x00")), git.ErrInvalidRequest},
		{"push", []byte(pkt("git-receive-pack /repo.git\x00host=localhost\x00")), ErrPushNotSupported},
	}
	for _, tc := range cases {
		line, err := rawRequest(t, tc.raw)
		if err == nil {
			t.Errorf("%s: expected ERR %q, got line %q", tc.name, tc.want, line)
			continue
		}
		wantErrLine(t, tc.name, err, tc.want)
	}
}

// TestRefusalSurvivesLateInput verifies a client that sends more after its
// request line, as git archive sends its arguments, still reads the refusal
// when that input arrives after the daemon has answered. The daemon used to
// close the connection at once, so the late input drew a reset: the client's
// next write failed, or its read lost the ERR packet to the reset.
func TestRefusalSurvivesLateInput(t *testing.T) {
	conns := make(chan *recordedConn, 1)
	_, addr := startDaemonOn(t, 1, func(inner net.Listener) net.Listener {
		return &recordingListener{Listener: inner, conns: conns}
	}, func(d *GitDaemon) {
		// The daemon waits for the client to hang up however late the
		// rest of the request comes.
		d.refuseLinger = time.Hour
	})
	c := dialDaemonAt(t, addr)
	request := pkt("git-upload-archive /missing\x00host=127.0.0.1\x00")
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	var conn *recordedConn
	select {
	case conn = <-conns:
	case <-time.After(idleWait):
		t.Fatal("the daemon did not accept the connection")
	}
	// The rest of the request goes out once the daemon has written its
	// answer and is done writing.
	select {
	case <-conn.shut:
	case <-conn.closed:
		t.Fatal("the daemon closed the connection at once after its answer")
	case <-time.After(idleWait):
		t.Fatal("the daemon did not answer")
	}
	sent := len(request)
	for _, p := range []string{pkt("argument --format=tar\n"), pkt("argument HEAD\n"), "0000"} {
		if _, err := io.WriteString(c, p); err != nil {
			t.Fatalf("write after the refusal failed: %v", err)
		}
		sent += len(p)
	}
	conn.waitRead(t, sent)
	line, err := readPktline(c)
	if err == nil {
		t.Fatalf("expected ERR %q, got line %q", git.ErrNotAuthed, line)
	}
	wantErrLine(t, "late input", err, git.ErrNotAuthed)
}

// recordingListener is a listener whose connections record what the daemon
// does with them. It sends each one on conns.
type recordingListener struct {
	net.Listener
	conns chan<- *recordedConn
}

func (l *recordingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	rc := &recordedConn{Conn: c, readMore: make(chan struct{}, 1), shut: make(chan struct{}), closed: make(chan struct{})}
	l.conns <- rc
	return rc, nil
}

// recordedConn is a connection that counts the bytes read from it, and
// closes shut once its write side is shut and closed once it is closed.
type recordedConn struct {
	net.Conn
	read     atomic.Int64
	readMore chan struct{}
	shut     chan struct{}
	closed   chan struct{}
	shutOnce sync.Once
	once     sync.Once
}

func (c *recordedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Add(int64(n))
		select {
		case c.readMore <- struct{}{}:
		default:
		}
	}
	return n, err
}

// waitRead waits for the daemon to read n bytes from c. It fails the test if
// the daemon closes c first: input that reaches a closed connection draws a
// reset, which the client's next write or read gets instead of the answer.
func (c *recordedConn) waitRead(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(idleWait)
	for c.read.Load() < int64(n) {
		select {
		case <-c.readMore:
		case <-c.closed:
			t.Fatalf("the daemon closed the connection after reading %d of the %d bytes sent", c.read.Load(), n)
		case <-deadline:
			t.Fatalf("the daemon read %d of the %d bytes sent", c.read.Load(), n)
		}
	}
}

func (c *recordedConn) CloseWrite() error {
	defer c.shutOnce.Do(func() { close(c.shut) })
	return c.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

func (c *recordedConn) Close() error {
	defer c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// TestRefusalReleasesConnection verifies a refused client that neither sends
// nor hangs up does not keep its connection past the refusal's linger.
func TestRefusalReleasesConnection(t *testing.T) {
	d, addr := startLimitedDaemon(t, 1)
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if _, err := io.WriteString(c, pkt("git-receive-pack /repo.git\x00host=localhost\x00")); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	_, err = readPktline(c)
	wantErrLine(t, "push", err, ErrPushNotSupported)
	// The connection stays open on this side; the daemon must still let
	// it go once the linger is over.
	deadline := time.Now().Add(refuseLinger + idleWait)
	for d.conns.Size() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("refused connection still open after %v", refuseLinger+idleWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProtocolVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		params string
		want   string
	}{
		{"", ""},
		{"\x00", ""},
		{"\x00version=2\x00", "2"},
		{"\x00version=1\x00version=2\x00", "2"},
		{"\x00version=2\x00version=1\x00", "2"},
		{"\x00bare-key\x00version=1\x00other=x\x00", "1"},
		{"\x00version=\x00", ""},
		{"\x00version=two\x00", ""},
		{"\x00version=-1\x00", ""},
		{"\x00version=2:evil=1\x00", ""},
		{"\x00version=99999\x00", ""},
		{"\x00foo=bar\x00", ""},
	}
	for _, tc := range cases {
		if got := protocolVersion([]byte(tc.params)); got != tc.want {
			t.Errorf("protocolVersion(%q) = %q, want %q", tc.params, got, tc.want)
		}
	}
}

// clientGoroutines returns how many goroutines are still running code of
// handleClient, including goroutines it started.
func clientGoroutines(t *testing.T) int {
	t.Helper()
	var buf strings.Builder
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		t.Fatalf("failed to dump goroutines: %v", err)
	}
	var n int
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "daemon.(*GitDaemon).handleClient") {
			n++
		}
	}
	return n
}

// TestFailedRequestDoesNotLeak checks that a connection that fails before
// sending its request, by timing out or by being reset, leaves no goroutine
// behind. Reading the request used to run on a goroutine that sent its result
// twice into a channel with room for one, and blocked forever once the
// handler had stopped listening.
func TestFailedRequestDoesNotLeak(t *testing.T) {
	addr := startIdleDaemon(t)
	var dialer net.Dialer
	dial := func() *net.TCPConn {
		t.Helper()
		c, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("failed to connect to daemon: %v", err)
		}
		return c.(*net.TCPConn)
	}

	// Idle until the daemon gives up on the request.
	idle := dial()
	defer idle.Close() //nolint:errcheck
	_ = idle.SetDeadline(time.Now().Add(idleWait))
	if _, err := readPktline(idle); err == nil || !strings.Contains(err.Error(), git.ErrTimeout.Error()) {
		t.Errorf("idle connection: expected %q, got %v", git.ErrTimeout, err)
	}

	// Reset the connection before sending anything.
	for range 5 {
		c := dial()
		_ = c.SetLinger(0)
		_ = c.Close()
	}

	deadline := time.Now().Add(idleWait)
	for {
		n := clientGoroutines(t)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines still in handleClient after their connections ended", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requestUploadPack asks the daemon for a repository and returns the first
// line it answers with, or the error it answers with.
func requestUploadPack(t *testing.T, repo string) (string, error) {
	t.Helper()
	return rawRequest(t, []byte(pkt("git-upload-pack /"+repo+".git\x00")))
}

// rawRequest sends raw bytes to the test daemon and returns the first line
// it answers with, or the error it answers with.
func rawRequest(t *testing.T, raw []byte) (string, error) {
	t.Helper()
	return rawRequestTo(t, testDaemon.addr, raw)
}

// rawRequestTo sends raw bytes to the daemon at addr and returns the first
// line it answers with, or the error it answers with. It closes the
// connection once it has the answer, so that a test making many requests
// does not hold one of the daemon's connections for each until it ends.
func rawRequestTo(t *testing.T, addr string, raw []byte) (string, error) {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := c.SetReadDeadline(time.Now().Add(idleWait)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	if _, err := c.Write(raw); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	return readPktline(c)
}

// refAdvertisement matches the first line of an upload-pack reference
// advertisement: an object id, a space, and a ref name (or capabilities^{}
// for an empty repository).
var refAdvertisement = regexp.MustCompile(`^[0-9a-f]{40} \S+\x00`)

// TestInvalidRepo verifies the daemon, which is always anonymous, answers a
// missing repository and a private one identically so neither is revealed,
// and serves a public one.
func TestInvalidRepo(t *testing.T) {
	ctx := context.Background()
	if _, err := testBackend.CreateRepository(ctx, "private", fixtureAdmin(t, testBackend), proto.RepositoryOptions{Private: true}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create private repo: %v", err)
	}

	if _, err := testBackend.CreateRepository(ctx, "public", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create public repo: %v", err)
	}

	for _, tc := range []struct {
		what, repo string
	}{
		{"missing repo", "test"},
		{"private repo", "private"},
		// Dot-dot segments are resolved inside the repositories
		// directory, so this names the missing repository "escape".
		{"dot-dot path", "../escape"},
		// ... and this one names "private".
		{"dot-dot path to a private repo", "x/../../private"},
	} {
		line, err := requestUploadPack(t, tc.repo)
		if err == nil {
			t.Errorf("%s: expected ERR %q, got line %q", tc.what, git.ErrNotAuthed, line)
			continue
		}
		wantErrLine(t, tc.what, err, git.ErrNotAuthed)
	}

	// A public repository is served: the daemon advertises its references,
	// also when the path reaches it through dot-dot segments.
	for _, repo := range []string{"public", "../public", "x/../../public"} {
		line, err := requestUploadPack(t, repo)
		if err != nil {
			t.Errorf("public repo %q was refused: %v", repo, err)
			continue
		}
		if !refAdvertisement.MatchString(line) {
			t.Errorf("public repo %q: expected a reference advertisement, got %q", repo, line)
		}
	}
}

// readAll sends a request to the daemon and returns everything it answers
// with until it closes the connection.
func readAll(t *testing.T, request ...string) []byte {
	t.Helper()
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", testDaemon.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := c.SetDeadline(time.Now().Add(idleWait)); err != nil {
		t.Fatalf("failed to set deadline: %v", err)
	}
	for _, r := range request {
		if _, err := io.WriteString(c, r); err != nil {
			t.Fatalf("failed to write request: %v", err)
		}
	}
	out, err := io.ReadAll(c)
	if err != nil && !isReset(err) {
		t.Fatalf("failed to read answer: %v", err)
	}
	return out
}

// isReset reports whether err is the connection being reset, which a server
// that closes with unread input can cause instead of a clean end.
func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || strings.Contains(err.Error(), "forcibly closed")
}

func pkt(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

// TestGitStderrNotSentToClient verifies that git's stderr and its error are
// kept out of the protocol stream. A protocol v2 request for an unknown
// command makes git print "fatal: invalid command" and exit with an error
// after it has advertised its capabilities.
func TestGitStderrNotSentToClient(t *testing.T) {
	if _, err := testBackend.CreateRepository(t.Context(), "stderr", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create repo: %v", err)
	}
	out := readAll(t,
		pkt("git-upload-pack /stderr.git\x00host=localhost\x00\x00version=2\x00"),
		pkt("command=foobar\n"), "0000",
	)
	if !bytes.Contains(out, []byte("version 2")) {
		t.Fatalf("expected a protocol v2 capability advertisement, got %q", out)
	}
	for _, s := range []string{"fatal", "invalid command", "ERR", "exit status"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("answer contains %q: %q", s, out)
		}
	}
	// Everything the daemon sent must parse as pkt-lines.
	for r := bytes.NewReader(out); ; {
		_, err := git.ReadPkt(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Errorf("answer is not a valid pkt-line stream: %v: %q", err, out)
			break
		}
	}
}

// TestGitFailureFixedMessage verifies that when git fails before answering,
// the client gets a fixed error message instead of git's output and error.
func TestGitFailureFixedMessage(t *testing.T) {
	if _, err := testBackend.CreateRepository(t.Context(), "broken", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create repo: %v", err)
	}
	// Leave an empty directory in place of the repository, which git
	// refuses as not a git repository.
	dir := filepath.Join(testDaemon.cfg.DataPath, "repos", "broken.git")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := readAll(t, pkt("git-upload-pack /broken.git\x00host=localhost\x00"))
	want := pkt("ERR " + git.ErrSystemMalfunction.Error() + "\n")
	if !bytes.HasPrefix(out, []byte(want)) {
		t.Errorf("expected %q, got %q", want, out)
	}
	for _, s := range []string{"fatal", "not a git repository", "exit status"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("answer contains %q: %q", s, out)
		}
	}
}

// Serve and Shutdown can run at the same time: Serve either sees the daemon
// closed or is waited for. Serve used to start counting on the WaitGroup
// that Shutdown might already be waiting on, which the race detector reports.
func TestServeRacingShutdown(t *testing.T) {
	ctx := config.WithContext(context.Background(), testDaemon.cfg)
	ctx = backend.WithContext(ctx, testBackend)
	for i := range 100 {
		d, err := NewGitDaemon(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- d.Serve(l) }()

		sctx, cancel := context.WithTimeout(context.Background(), idleWait)
		err = d.Shutdown(sctx)
		cancel()
		if err != nil {
			t.Fatalf("run %d: Shutdown: %v", i, err)
		}
		_ = l.Close() // Serve may not have registered it before Shutdown
		select {
		case err := <-served:
			if !errors.Is(err, ErrServerClosed) {
				t.Fatalf("run %d: Serve returned %v, want ErrServerClosed", i, err)
			}
		case <-time.After(idleWait):
			t.Fatalf("run %d: Serve did not return after Shutdown", i)
		}
	}
}

// TestListensOnLoopback checks that the test daemon binds only loopback.
// RandomPort checks that a port is free on loopback alone, so a daemon bound
// to every interface could collide with a listener on another one.
func TestListensOnLoopback(t *testing.T) {
	host, _, err := net.SplitHostPort(testDaemon.addr)
	if err != nil {
		t.Fatalf("invalid daemon address %q: %v", testDaemon.addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("daemon listens on %q, want a loopback address", testDaemon.addr)
	}
}

// TestUploadPackCounted checks that the daemon counts each upload-pack
// request it serves once, under its own metric name, and does not count a
// request it refuses.
func TestUploadPackCounted(t *testing.T) {
	ctx := context.Background()
	// The daemon and its backend are shared by every run of the test, so
	// the repositories may exist already, and counters are compared with
	// their values before the requests.
	if _, err := testBackend.CreateRepository(ctx, "counted", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create repo: %v", err)
	}
	if _, err := testBackend.CreateRepository(ctx, "counted-private", fixtureAdmin(t, testBackend), proto.RepositoryOptions{Private: true}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create private repo: %v", err)
	}

	served := testutil.ToFloat64(uploadPackGitCounter.WithLabelValues("counted"))
	refused := testutil.ToFloat64(uploadPackGitCounter.WithLabelValues("counted-private"))
	for range 2 {
		if _, err := requestUploadPack(t, "counted"); err != nil {
			t.Fatalf("public repo was refused: %v", err)
		}
	}
	if _, err := requestUploadPack(t, "counted-private"); err == nil {
		t.Fatal("private repo was served")
	}

	if got := testutil.ToFloat64(uploadPackGitCounter.WithLabelValues("counted")) - served; got != 2 {
		t.Errorf("upload-pack counter for a served repo = %v, want 2", got)
	}
	if got := testutil.ToFloat64(uploadPackGitCounter.WithLabelValues("counted-private")) - refused; got != 0 {
		t.Errorf("upload-pack counter for a refused repo = %v, want 0", got)
	}

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	const want = "soft_serve_daemon_git_upload_pack_total"
	for _, mf := range mfs {
		if mf.GetName() == want {
			return
		}
	}
	t.Errorf("metric %s is not registered", want)
}

// startLimitedDaemon serves a daemon allowing limit connections that never
// time out, and returns it with its address. opts change the daemon before
// it serves.
func startLimitedDaemon(t *testing.T, limit int, opts ...func(*GitDaemon)) (*GitDaemon, string) {
	t.Helper()
	return startDaemonOn(t, limit, nil, opts...)
}

// startDaemonOn is startLimitedDaemon serving on the listener wrap returns
// for a loopback one, or on the loopback one if wrap is nil.
func startDaemonOn(t *testing.T, limit int, wrap func(net.Listener) net.Listener, opts ...func(*GitDaemon)) (*GitDaemon, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = testDaemon.cfg.DataPath
	cfg.Git.MaxConnections = limit
	cfg.Git.MaxTimeout = 0
	cfg.Git.IdleTimeout = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = backend.WithContext(ctx, testBackend)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Kept connections send no request line; do not drop them for that.
	patient(d)
	for _, opt := range opts {
		opt(d)
	}
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		l = wrap(l)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(l) }()
	t.Cleanup(func() {
		_ = d.Close()
		if err := <-served; !errors.Is(err, ErrServerClosed) {
			t.Errorf("Serve returned %v, want ErrServerClosed", err)
		}
	})
	return d, l.Addr().String()
}

// probeRequest asks for a repository that does not exist. A kept connection
// sends it to show that the daemon serves it: the answer is
// git.ErrNotAuthed, never the refusal.
var probeRequest = pkt("git-upload-pack /missing\x00host=127.0.0.1\x00")

// wantServed fails the test unless the daemon answers a request on c, which
// it has kept, as it answers any client.
func wantServed(t *testing.T, what string, c net.Conn) {
	t.Helper()
	if _, err := io.WriteString(c, probeRequest); err != nil {
		t.Fatalf("%s: write request: %v", what, err)
	}
	_, err := readPktline(c)
	wantErrLine(t, what, err, git.ErrNotAuthed)
}

// isRefusal reports whether err, from reading a connection's first answer,
// is a refusal for the connection limit: the refusal itself, or, for one of
// the connections the daemon closes at once past maxRefusing refusals in
// progress, the end of the stream or a reset that cost the client the
// refusal. A kept connection gets none of these before it sends a request.
func isRefusal(err error) bool {
	var errLine *git.PktlineError
	if errors.As(err, &errLine) {
		return errLine.Text == git.ErrMaxConnections.Error()
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isReset(err)
}

// TestMaxConnections checks that max_connections N lets N connections in and
// refuses the next. It used to let only N-1 in, and a limit of 1 none.
//
// Nothing is decided by a read that did not end in time. A connection is
// kept once the daemon counts it, and still served: its request gets an
// answer. The one past the limit reads the refusal and then the end of the
// stream. Each is waited for up to idleWait.
func TestMaxConnections(t *testing.T) {
	for _, limit := range []int{1, 3} {
		d, addr := startLimitedDaemon(t, limit)
		kept := make([]net.Conn, limit)
		for i := range limit {
			kept[i] = holdSlot(t, d, addr)
		}
		over := dialHeld(t, addr, "", git.ErrMaxConnections)
		if out, err := io.ReadAll(over); err != nil || (len(out) != 0 && string(out) != "0000") {
			t.Fatalf("limit %d: after the refusal: read %q, %v; want the end of the stream", limit, out, err)
		}
		if got := d.conns.Size(); got != limit {
			t.Fatalf("limit %d: %d connections open", limit, got)
		}
		for i, c := range kept {
			wantServed(t, fmt.Sprintf("limit %d: connection %d", limit, i+1), c)
		}
	}
}

// TestMaxConnectionsBurst dials many connections at once: exactly limit of
// them are kept. Connections used to be counted in their own goroutines, so
// a burst passed the check before any was counted.
//
// The test waits, up to idleWait, until all but limit of the connections are
// refused, then checks that each of the others is still served. A
// connection is never taken for kept because its refusal was slow to come.
func TestMaxConnectionsBurst(t *testing.T) {
	const limit, dials = 3, 64
	d, addr := startLimitedDaemon(t, limit)
	dialed := make(chan net.Conn, dials)
	for range dials {
		go func() {
			c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
			if err != nil {
				t.Logf("dial: %v", err)
				c = nil
			}
			dialed <- c
		}()
	}
	// A burst of loopback dials can time out on macOS when the listen
	// backlog drops SYNs, without the daemon being involved. Such a dial
	// never reaches the daemon, so skip it and check the limit over the
	// connections that did get through.
	var conns []net.Conn
	for range dials {
		if c := <-dialed; c != nil {
			t.Cleanup(func() { _ = c.Close() })
			conns = append(conns, c)
		}
	}
	if len(conns) <= limit {
		t.Skipf("only %d of %d dials connected, need more than %d", len(conns), dials, limit)
	}

	// Read each connection's first answer.
	type answer struct {
		i   int
		err error
	}
	answers := make(chan answer, len(conns))
	for i, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(2 * idleWait))
		go func() {
			_, err := readPktline(c)
			answers <- answer{i, err}
		}()
	}

	wantRefused := len(conns) - limit
	answered := make(map[int]bool)
	timeout := time.After(idleWait)
	for len(answered) < wantRefused {
		select {
		case a := <-answers:
			if !isRefusal(a.err) {
				t.Fatalf("connection %d answered %v before sending a request, want a refusal or no answer", a.i, a.err)
			}
			answered[a.i] = true
		case <-timeout:
			t.Fatalf("%d of %d connections refused after %s, want %d", len(answered), len(conns), idleWait, wantRefused)
		}
	}
	waitConns(t, d, limit, idleWait)

	// The rest are kept: each still gets an answer to its request.
	for i, c := range conns {
		if answered[i] {
			continue
		}
		if _, err := io.WriteString(c, probeRequest); err != nil {
			t.Fatalf("connection %d: write request: %v", i, err)
		}
		a := <-answers
		if a.i != i {
			t.Fatalf("connection %d answered %v while connection %d waited for an answer", a.i, a.err, i)
		}
		wantErrLine(t, fmt.Sprintf("kept connection %d", i), a.err, git.ErrNotAuthed)
	}
}

// TestConnectionsAddAfterCloseAll checks that a connection accepted while the
// daemon closes is not added after CloseAll and so escapes the forced close.
func TestConnectionsAddAfterCloseAll(t *testing.T) {
	m := newConnections()
	a, b := net.Pipe()
	defer a.Close() //nolint:errcheck
	defer b.Close() //nolint:errcheck
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(a, 10, 0); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Add after CloseAll = %v, want ErrServerClosed", err)
	}
	if got := m.Size(); got != 0 {
		t.Fatalf("%d connections after CloseAll, want 0", got)
	}
}

// TestRequestTimeout checks that a client which sends part of its request
// line, well within the idle timeout, is still dropped once the request
// timeout passes, and is told why. Every read used to push the deadline
// forward, so a client trickling its request a byte at a time could hold a
// connection slot indefinitely.
//
// The idle timeout is ten minutes, far past the test's wait, so an answer
// within the wait can only come from the request timeout and the test need
// not time it.
func TestRequestTimeout(t *testing.T) {
	const requestTimeout = 2 * time.Second
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 600
	ctx := config.WithContext(context.Background(), &cfg)
	ctx = backend.WithContext(ctx, testBackend)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d.requestTimeout = requestTimeout
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })

	// Start a request line whose declared length is more than is ever
	// sent. The daemon reads it, which used to push the deadline past the
	// idle timeout. A client slowed past the request timeout before it
	// writes can have its write reset the connection the daemon has just
	// closed, losing the answer; it tries again on a new connection.
	waitForDaemon(t, "the daemon to answer a partial request line", func() bool {
		c := dialDaemonAt(t, l.Addr().String())
		defer c.Close() //nolint:errcheck
		if _, err := io.WriteString(c, "0fffgit"); err != nil {
			t.Logf("write: %v; trying again", err)
			return false
		}
		// The scanner reports an ERR packet as an error carrying its message.
		line, err := readPktline(c)
		if err == nil {
			t.Fatalf("expected a timeout error, got %q", line)
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("the daemon kept a partial request line open for %s", idleWait)
		}
		if !strings.Contains(err.Error(), git.ErrTimeout.Error()) {
			if isReset(err) {
				t.Logf("read: %v; trying again", err)
				return false
			}
			t.Fatalf("expected the daemon to report %q, got %v", git.ErrTimeout, err)
		}
		return true
	})
}

// waitForDaemon calls cond until it holds, failing the test after idleWait.
func waitForDaemon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(idleWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKeepAlive(t *testing.T) {
	for _, c := range []struct {
		idle time.Duration
		want int
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Second, 1},
		{2 * time.Second, 1},
		{3 * time.Second, 1},
		{10 * time.Second, 5},
		{600 * time.Second, 300},
	} {
		if got := keepAlive(c.idle); got != c.want {
			t.Errorf("keepAlive(%v) = %d, want %d", c.idle, got, c.want)
		}
	}
}

// ensureRepo creates the public repository name unless it exists, as it
// does when the tests run more than once.
func ensureRepo(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testBackend.Repository(ctx, name); err == nil {
		return
	}
	if _, err := testBackend.CreateRepository(ctx, name, fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
}

// runGit runs git with args in dir and fails the test if it fails.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestQuietCloneSlowPack clones quietly from a repository whose pack takes
// longer than the idle timeout to start. Neither side talks meanwhile, so
// the daemon has upload-pack send keepalives; without them the connection
// was closed as idle, and the ERR packet written after it broke the clone
// with "bad band".
//
// No clock decides the outcome. The pack waits until the test releases it,
// once a keepalive has reached the connection, and the connection's own
// deadlines never pass (see stillConn), so neither a stalled daemon nor a
// stalled upload-pack closes it. The daemon must ask upload-pack for
// keepalives at keepAlive's interval, which is shorter than the idle
// timeout, rather than at git's default of five seconds, which is longer.
func TestQuietCloneSlowPack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pack-objects hook needs a POSIX shell")
	}
	const name = "slowpack"
	ensureRepo(t, name)
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "README")
	runGit(t, src, "commit", "-q", "-m", "init")
	bare := filepath.Join(testDaemon.cfg.DataPath, "repos", name+".git")
	head := runGit(t, bare, "symbolic-ref", "HEAD")
	// Fetch into the repository rather than push, which would run the
	// server's hooks.
	runGit(t, bare, "fetch", "-q", src, "+HEAD:"+head)

	const idleTimeout = 2
	conns := make(chan *stillConn, 1)
	_, addr := startDaemonOn(t, 1, stillConns(t, conns), func(d *GitDaemon) { d.cfg.Git.IdleTimeout = idleTimeout })

	// Hold the pack back until the test releases it. The hook records
	// the keepalive interval upload-pack runs with and that the pack is
	// held. It is only honoured from protected configuration, which the
	// command-line scope of GIT_CONFIG_COUNT is; the daemon passes its
	// environment to git.
	dir := t.TempDir()
	interval, held, release := filepath.Join(dir, "interval"), filepath.Join(dir, "held"), filepath.Join(dir, "release")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "uploadpack.packObjectsHook")
	t.Setenv("GIT_CONFIG_VALUE_0", fmt.Sprintf(
		"git config --show-scope --get uploadpack.keepalive >'%s'; : >'%s'; while test ! -e '%s'; do sleep 1; done;",
		interval, held, release))

	dst := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "-q", "git://"+addr+"/"+name, dst)
	type result struct {
		out []byte
		err error
	}
	cloned := make(chan result, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		cloned <- result{out, err}
	}()
	// The pack is held until the release file exists, so release it
	// however the test ends.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	var c *stillConn
	select {
	case c = <-conns:
	case r := <-cloned:
		t.Fatalf("clone ended before the daemon accepted it: %v\n%s", r.err, r.out)
	case <-time.After(idleWait):
		t.Fatal("the daemon did not accept the clone")
	}
	waitForDaemon(t, "upload-pack to hold the pack", func() bool {
		_, err := os.Stat(held)
		return err == nil
	})
	got, err := os.ReadFile(interval)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("command\t%d\n", keepAlive(idleTimeout*time.Second)); string(got) != want {
		t.Fatalf("upload-pack's keepalive interval: %q, want %q", got, want)
	}
	// An empty data packet is upload-pack's keepalive.
	waitForDaemon(t, "a keepalive while the pack is held", func() bool {
		return c.wrote([]byte("0005\x01"))
	})
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The connection keeps no deadlines, so the wait for the clone has
	// to have its own.
	select {
	case r := <-cloned:
		if r.err != nil {
			t.Fatalf("clone failed: %v\n%s", r.err, r.out)
		}
	case <-time.After(idleWait):
		t.Fatalf("the clone did not finish %s after the pack was released", idleWait)
	}
	if _, err := os.Stat(filepath.Join(dst, "README")); err != nil {
		t.Errorf("clone is missing README: %v", err)
	}
}

// TestNoErrAfterOutput times a request out after upload-pack answered with
// its references. The daemon closes the connection, but must not write an
// ERR packet: the client already reads the response, so it would take the
// packet for part of it.
//
// The test times the request out itself once it has read the first
// reference (see stillConn), so the timeout comes after the output has
// started however slow either side is, and the output starts however slow
// git is.
func TestNoErrAfterOutput(t *testing.T) {
	const name = "idleafteradvert"
	ensureRepo(t, name)
	conns := make(chan *stillConn, 1)
	_, addr := startDaemonOn(t, 1, stillConns(t, conns))
	c := dialDaemonAt(t, addr)
	if _, err := io.WriteString(c, pkt("git-upload-pack /"+name+".git\x00")); err != nil {
		t.Fatal(err)
	}
	if line, err := readPktline(c); err != nil || !refAdvertisement.MatchString(line) {
		t.Fatalf("first answer: %q, %v; want a reference advertisement", line, err)
	}
	var sc *stillConn
	select {
	case sc = <-conns:
	case <-time.After(idleWait):
		t.Fatal("the daemon did not accept the connection")
	}
	// Stay silent and time the connection out, as the idle timeout
	// would, then collect everything else the daemon sent.
	sc.expire()
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if strings.Contains(string(out), "ERR ") {
		t.Errorf("response has an ERR packet after output started:\n%q", out)
	}
}

// stillConns returns a listener wrapper for startDaemonOn that makes each
// connection a stillConn and sends it on conns, which has room for every
// connection the test expects; see stillListener.
func stillConns(t *testing.T, conns chan<- *stillConn) func(net.Listener) net.Listener {
	return func(inner net.Listener) net.Listener {
		return &stillListener{Listener: inner, conns: conns, t: t}
	}
}

// stillListener is a listener whose connections are stillConns. It sends
// each one on conns. A connection that finds conns full is one the test
// did not expect: it fails the test, and is served all the same, so that
// the accept loop, and with it the daemon's shutdown, never waits on conns.
type stillListener struct {
	net.Listener
	conns chan<- *stillConn
	t     *testing.T
}

func (l *stillListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &stillConn{Conn: c}
	select {
	case l.conns <- sc:
	default:
		l.t.Errorf("unexpected connection from %s", c.RemoteAddr())
	}
	return sc, nil
}

// stillConn is a connection whose idle and other timeouts never pass by
// themselves: the deadlines the daemon sets through SetDeadline are dropped.
// A test times the connection out with expire instead, at a point of its
// choosing, so no timeout is a race with how long the daemon, git or the
// test take. It records what the daemon writes.
type stillConn struct {
	net.Conn
	mu      sync.Mutex
	expired bool
	written []byte
}

// SetDeadline drops t. Once the connection has expired, reads keep failing
// with a timeout, as they do once a real deadline has passed; a write
// still goes through, as it does when the daemon moves its deadline
// forward to write.
func (c *stillConn) SetDeadline(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return c.Conn.SetWriteDeadline(time.Time{})
	}
	return c.Conn.SetDeadline(time.Time{})
}

// expire times the connection out: a read in progress and every later one
// fail with a timeout.
func (c *stillConn) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expired = true
	_ = c.Conn.SetReadDeadline(aLongTimeAgo)
}

func (c *stillConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.written = append(c.written, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// wrote reports whether what the daemon wrote holds b.
func (c *stillConn) wrote(b []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Contains(c.written, b)
}

// NetConn returns the wrapped connection, so the daemon can still shut its
// write side when it refuses the client.
func (c *stillConn) NetConn() net.Conn { return c.Conn }

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}
