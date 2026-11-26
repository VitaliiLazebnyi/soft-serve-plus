package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/internal/backendtest"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/gitserve"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	_ "modernc.org/sqlite" // sqlite driver
)

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
func patient(d *Server) {
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
	d, err := NewServer(&cfg, testBackend, nil)
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
// *gitserve.PktlineError carrying the daemon's message.
func readPktline(c net.Conn) (string, error) {
	line, err := gitserve.ReadPkt(c)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(line)), nil
}

// wantErrLine fails the test unless err is an ERR packet carrying want.
func wantErrLine(t *testing.T, what string, err error, want error) {
	t.Helper()
	var errLine *gitserve.PktlineError
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
	wantErrLine(t, "idle connection", err, gitserve.ErrTimeout)
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
		{"invalid pkt-len", []byte("zzzz"), gitserve.ErrInvalidRequest},
		{"client error packet", []byte(pkt("ERR boom\n")), gitserve.ErrInvalidRequest},
		{"unknown service", []byte(pkt("git-frobnicate /repo.git\x00")), gitserve.ErrInvalidRequest},
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
// when that input arrives after the daemon has answered. A daemon that
// closed the connection at once would draw a reset with the late input: the
// client's next write would fail, or its read would lose the ERR packet.
func TestRefusalSurvivesLateInput(t *testing.T) {
	conns := make(chan *recordedConn, 1)
	_, addr := startDaemonOn(t, 1, func(inner net.Listener) net.Listener {
		return &recordingListener{Listener: inner, conns: conns}
	}, func(d *Server) {
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
		t.Fatalf("expected ERR %q, got line %q", gitserve.ErrNotAuthed, line)
	}
	wantErrLine(t, "late input", err, gitserve.ErrNotAuthed)
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

// The daemon reads the protocol version as git reads GIT_PROTOCOL, which is
// how SSH and HTTP read the client's value too.
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
		// git-daemon joins the parameters with colons into GIT_PROTOCOL,
		// so git reads this one as version=2; evil=1 goes no further.
		{"\x00version=2:evil=1\x00", "2"},
		{"\x00version=99999\x00", ""},
		// git ignores a version it does not know.
		{"\x00version=3\x00", ""},
		{"\x00version=1\x00version=3\x00", "1"},
		{"\x00version=02\x00", ""},
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
		if strings.Contains(g, "daemon.(*Server).handleClient") {
			n++
		}
	}
	return n
}

// TestFailedRequestDoesNotLeak checks that a connection that fails before
// sending its request, by timing out or by being reset, leaves no goroutine
// behind.
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
	if _, err := readPktline(idle); err == nil || !strings.Contains(err.Error(), gitserve.ErrTimeout.Error()) {
		t.Errorf("idle connection: expected %q, got %v", gitserve.ErrTimeout, err)
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
			t.Errorf("%s: expected ERR %q, got line %q", tc.what, gitserve.ErrNotAuthed, line)
			continue
		}
		wantErrLine(t, tc.what, err, gitserve.ErrNotAuthed)
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
		_, err := gitserve.ReadPkt(r)
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
	want := pkt("ERR " + gitserve.ErrSystemMalfunction.Error() + "\n")
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
// closed or is waited for: it does not add to a WaitGroup that Shutdown may
// already be waiting on, which the race detector reports.
func TestServeRacingShutdown(t *testing.T) {
	for i := range 100 {
		d, err := NewServer(testDaemon.cfg, testBackend, nil)
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
func startLimitedDaemon(t *testing.T, limit int, opts ...func(*Server)) (*Server, string) {
	t.Helper()
	return startDaemonOn(t, limit, nil, opts...)
}

// startDaemonOn is startLimitedDaemon serving on the listener wrap returns
// for a loopback one, or on the loopback one if wrap is nil.
func startDaemonOn(t *testing.T, limit int, wrap func(net.Listener) net.Listener, opts ...func(*Server)) (*Server, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.DataPath = testDaemon.cfg.DataPath
	cfg.Git.MaxConnections = limit
	cfg.Git.MaxTimeout = 0
	cfg.Git.IdleTimeout = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := NewServer(cfg, testBackend, nil)
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
// gitserve.ErrNotAuthed, never the refusal.
var probeRequest = pkt("git-upload-pack /missing\x00host=127.0.0.1\x00")

// wantServed fails the test unless the daemon answers a request on c, which
// it has kept, as it answers any client.
func wantServed(t *testing.T, what string, c net.Conn) {
	t.Helper()
	if _, err := io.WriteString(c, probeRequest); err != nil {
		t.Fatalf("%s: write request: %v", what, err)
	}
	_, err := readPktline(c)
	wantErrLine(t, what, err, gitserve.ErrNotAuthed)
}

// isRefusal reports whether err, from reading a connection's first answer,
// is a refusal for the connection limit: the refusal itself, or, for one of
// the connections the daemon closes at once past maxRefusing refusals in
// progress, the end of the stream or a reset that cost the client the
// refusal. A kept connection gets none of these before it sends a request.
func isRefusal(err error) bool {
	var errLine *gitserve.PktlineError
	if errors.As(err, &errLine) {
		return errLine.Text == gitserve.ErrMaxConnections.Error()
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isReset(err)
}

// TestMaxConnections checks that max_connections N lets N connections in and
// refuses the next, and a limit of 1 lets one in.
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
		over := dialHeld(t, addr, "", gitserve.ErrMaxConnections)
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
// them are kept: each connection is counted before the next is checked.
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
		wantErrLine(t, fmt.Sprintf("kept connection %d", i), a.err, gitserve.ErrNotAuthed)
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
// timeout passes, and is told why: a read does not push the deadline
// forward, so a client trickling its request a byte at a time cannot hold a
// connection slot indefinitely.
//
// The idle timeout is ten minutes, far past the test's wait, so an answer
// within the wait can only come from the request timeout and the test need
// not time it.
func TestRequestTimeout(t *testing.T) {
	const requestTimeout = 2 * time.Second
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 600
	d, err := NewServer(&cfg, testBackend, nil)
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
	// sent. The daemon reads it, which must not push the deadline past
	// the idle timeout. A client slowed past the request timeout before it
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
		if !strings.Contains(err.Error(), gitserve.ErrTimeout.Error()) {
			if isReset(err) {
				t.Logf("read: %v; trying again", err)
				return false
			}
			t.Fatalf("expected the daemon to report %q, got %v", gitserve.ErrTimeout, err)
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
	_, addr := startDaemonOn(t, 1, stillConns(t, conns), func(d *Server) { d.cfg.Git.IdleTimeout = idleTimeout })

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

// startCloseTestDaemon serves a daemon like the test daemon whose request
// and idle timeouts are ten minutes, far past the tests' one-minute wait,
// and which has no total time limit on a connection, so a request that ends
// within the wait can only have been ended by Close. opts change the daemon
// before it serves.
func startCloseTestDaemon(t *testing.T, opts ...func(*Server)) (*Server, string) {
	t.Helper()
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 600
	cfg.Git.MaxTimeout = 0
	d, err := NewServer(&cfg, testBackend, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.requestTimeout = 10 * time.Minute
	for _, opt := range opts {
		opt(d)
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })
	return d, l.Addr().String()
}

// waitHandlers waits up to idleWait for every goroutine of d, its accept
// loop and its handlers, to return.
func waitHandlers(t *testing.T, d *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(idleWait):
		t.Fatal("a handler is still running after Close")
	}
}

// Close ends a request the daemon is still reading, and its handler
// returns, rather than waiting out the ten-minute request timeout: the
// daemon has no context of its own, so closing the connection is what ends
// it.
func TestCloseEndsRequestRead(t *testing.T) {
	d, addr := startCloseTestDaemon(t)
	dialDaemonAt(t, addr)
	// The daemon is reading the request once it counts the connection.
	waitConns(t, d, 1, idleWait)
	_ = d.Close()
	waitHandlers(t, d)
}

// Close ends the context of a request being served, so the git command and
// the database lookups it runs stop, and not only the connection's reads
// and writes.
func TestCloseEndsRequestContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	d, addr := startCloseTestDaemon(t, func(d *Server) {
		d.testHooks.beforeAuthorize = func(ctx context.Context) {
			close(started)
			<-release
			ctxErr <- ctx.Err()
		}
	})
	c := dialDaemonAt(t, addr)
	if _, err := c.Write([]byte(pkt("git-upload-pack /close-public.git\x00host=localhost\x00"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(idleWait):
		t.Fatal("the request was not served")
	}
	// The hook holds the handler while the daemon closes; it then reads
	// the context it was served with.
	_ = d.Close()
	close(release)
	if err := <-ctxErr; err == nil {
		t.Error("the request's context was not done after Close")
	}
	waitHandlers(t, d)
}

func TestEarliest(t *testing.T) {
	t.Parallel()
	base := time.Now()
	before, after := base.Add(-time.Second), base.Add(time.Second)
	for _, tc := range []struct {
		ts   []time.Time
		want time.Time
	}{
		{nil, base},
		{[]time.Time{{}, {}}, base},
		{[]time.Time{after}, base},
		{[]time.Time{{}, before}, before},
		{[]time.Time{after, before, {}}, before},
	} {
		if got := earliest(base, tc.ts...); !got.Equal(tc.want) {
			t.Errorf("earliest(base, %v) = %v, want %v", tc.ts, got, tc.want)
		}
	}
}

// TestRequestLineAnswers pins the answer to each kind of request line, as
// the client sees it. It has a daemon of its own with room for every case,
// so that connections still closing cannot crowd out the next case or the
// other tests' requests to the shared daemon.
func TestRequestLineAnswers(t *testing.T) {
	ensureRepo(t, "answers-public")
	cases := []struct {
		name string
		line string
		want error // nil: served
	}{
		{"no space", "git-upload-pack", gitserve.ErrInvalidRequest},
		{"no host parameter", "git-upload-pack /answers-public.git", gitserve.ErrInvalidRequest},
		{"unknown service", "git-upload-anything /answers-public.git\x00host=localhost\x00", gitserve.ErrInvalidRequest},
		{"push", "git-receive-pack /answers-public.git\x00host=localhost\x00", ErrPushNotSupported},
		{"missing repo", "git-upload-pack /answers-missing.git\x00host=localhost\x00", gitserve.ErrNotAuthed},
		{"missing repo, archive", "git-upload-archive /answers-missing.git\x00host=localhost\x00", gitserve.ErrNotAuthed},
		{"public repo", "git-upload-pack /answers-public.git\x00host=localhost\x00", nil},
		{"public repo without .git", "git-upload-pack /answers-public\x00host=localhost\x00", nil},
		{"public repo, empty host", "git-upload-pack /answers-public.git\x00\x00", nil},
		{"public repo, v2", "git-upload-pack /answers-public.git\x00host=localhost\x00\x00version=2\x00", nil},
	}
	_, addr := startLimitedDaemon(t, len(cases))
	for _, tc := range cases {
		line, err := rawRequestTo(t, addr, []byte(pkt(tc.line)))
		switch {
		case tc.want != nil:
			wantErrLine(t, tc.name, err, tc.want)
		case err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case line == "":
			t.Errorf("%s: empty answer", tc.name)
		}
	}
}

// TestUploadArchiveServed pins that the daemon serves upload-archive for a
// public repository: git acknowledges the archive arguments.
func TestUploadArchiveServed(t *testing.T) {
	ensureRepo(t, "archive-public")
	c := dialDaemon(t)
	defer c.Close() //nolint:errcheck
	for _, p := range []string{
		pkt("git-upload-archive /archive-public.git\x00host=localhost\x00"),
		pkt("argument --format=tar\n"), pkt("argument HEAD\n"), "0000",
	} {
		if _, err := io.WriteString(c, p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	line, err := readPktline(c)
	if err != nil {
		t.Fatalf("archive request refused: %v", err)
	}
	if line != "ACK" {
		t.Errorf("first line = %q, want ACK", line)
	}
}

// TestHangUpWithoutRequest pins that a client that hangs up before sending
// a request gets no answer and releases its connection.
func TestHangUpWithoutRequest(t *testing.T) {
	d, addr := startLimitedDaemon(t, 1)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil && !isReset(err) {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("answer to a client that sent nothing: %q", out)
	}
	waitConns(t, d, 0, idleWait)
}

// waitConns waits up to within until d holds n connections.
func waitConns(t *testing.T, d *Server, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for d.conns.Size() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections open after %v, want %d", d.conns.Size(), within, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitLog waits up to within until logs holds substr. A line the daemon
// writes after the client has seen the effect it goes with, such as a
// panic logged once its connection is closed, may not be there yet when
// the client sees that effect, so a test cannot read the logs just once.
func waitLog(t *testing.T, logs *syncBuffer, substr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(logs.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("%q not logged after %v; logs:\n%s", substr, within, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newLookupTestDaemon returns a daemon with its own database, which the
// lookup tests break: Postgres when SOFT_SERVE_DB_DRIVER is "postgres",
// SQLite otherwise. It holds one public repository, "public". The log is
// written to the returned buffer at Debug level.
func newLookupTestDaemon(t *testing.T) (context.Context, *Server, *db.DB, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := log.New(logs)
	logger.SetLevel(log.DebugLevel)
	e := backendtest.New(t, backendtest.WithLogger(logger))
	ctx, cfg, dbx, be := e.Ctx, e.Cfg, e.DB, e.Backend
	if _, err := be.CreateRepository(ctx, "public", fixtureAdmin(t, be), proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := NewServer(cfg, be, logger)
	if err != nil {
		t.Fatal(err)
	}
	patient(d)
	t.Cleanup(func() { _ = d.Close() })
	return ctx, d, dbx, logs
}

// A repository lookup that fails is a server fault: the daemon answers with
// its fixed malfunction message and logs the cause, rather than answering
// as for a missing or private repository. A missing name gets the same
// answer as an existing one, so an outage does not tell which names exist,
// and so does every name when the keyless setting cannot be read.
func TestAuthorizeLookupFailure(t *testing.T) {
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			ctx, d, dbx, logs := newLookupTestDaemon(t)
			public := gitRequest{service: gitserve.UploadPackService, name: "public"}
			if err := d.authorize(ctx, public); err != nil {
				t.Fatalf("authorize = %v, want nil", err)
			}
			if err := d.authorize(ctx, gitRequest{service: gitserve.UploadPackService, name: "ghost"}); !errors.Is(err, gitserve.ErrNotAuthed) {
				t.Fatalf("authorize(ghost) = %v, want %v", err, gitserve.ErrNotAuthed)
			}

			if _, err := dbx.ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"public", "ghost"} {
				if err := d.authorize(ctx, gitRequest{service: gitserve.UploadPackService, name: name}); !errors.Is(err, gitserve.ErrSystemMalfunction) {
					t.Fatalf("authorize(%s) with the lookup failing = %v, want %v", name, err, gitserve.ErrSystemMalfunction)
				}
			}
			if !strings.Contains(logs.String(), "failed to look up repository") {
				t.Errorf("logs = %q, want the failed lookup", logs.String())
			}
		})
	}
}

// A lookup canceled because the client went away is logged at Debug, not
// as a server error.
func TestAuthorizeLookupCanceled(t *testing.T) {
	ctx, d, _, logs := newLookupTestDaemon(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.authorize(cctx, gitRequest{service: gitserve.UploadPackService, name: "public"}); !errors.Is(err, gitserve.ErrSystemMalfunction) {
		t.Fatalf("authorize with the client gone = %v, want %v", err, gitserve.ErrSystemMalfunction)
	}
	out := logs.String()
	if !strings.Contains(out, "repository lookup canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled lookup at Debug only", out)
	}
}

// NewServer refuses a missing config or backend rather than build a
// daemon that fails at its first request.
func TestNewServerNilDependencies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		be   *backend.Backend
		want error
	}{
		{"nil config", nil, testBackend, config.ErrNilConfig},
		{"nil backend", testDaemon.cfg, nil, errNilBackend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := NewServer(tc.cfg, tc.be, nil)
			if !errors.Is(err, tc.want) || d != nil {
				t.Errorf("NewServer = %v, %v; want nil, %v", d, err, tc.want)
			}
		})
	}
}

// TestHandlerPanicRecovered checks that a panic while a connection is
// handled is logged and does not end the daemon: the connection is closed,
// its slot and its place in the WaitGroup are given back, and the next
// client is served. The handler's goroutine had no recover, so such a panic
// ended the whole server, and its wg.Done was not deferred.
func TestHandlerPanicRecovered(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	var panicked atomic.Bool
	d, addr := startLimitedDaemon(t, 1, func(d *Server) {
		d.logger = log.New(logs)
		d.testHooks.beforeAuthorize = func(context.Context) {
			if panicked.CompareAndSwap(false, true) {
				panic("injected handler panic")
			}
		}
	})
	request := pkt("git-upload-pack /panic-missing.git\x00host=localhost\x00")

	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// The daemon closes the connection without an answer.
	if out, err := io.ReadAll(c); len(out) != 0 || (err != nil && !isReset(err)) {
		t.Fatalf("after the panic: read %q, %v; want the connection closed", out, err)
	}
	if !panicked.Load() {
		t.Fatal("the hook did not run")
	}
	// The slot is free again: with a limit of 1, the next client is
	// served rather than refused.
	waitConns(t, d, 0, idleWait)
	c2 := dialHeld(t, addr, request, gitserve.ErrNotAuthed)
	_ = c2.Close()

	// The panic is logged by safego.Run's recover, which runs after
	// handleClient's deferred close: the client can see the connection
	// closed, and the next client be served, before the line is written.
	waitLog(t, logs, "recovered panic", idleWait)
	waitLog(t, logs, "injected handler panic", idleWait)
	// The handler's goroutine is not counted once it has recovered:
	// Shutdown does not wait for it.
	waitConns(t, d, 0, idleWait)
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}

func TestParseRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want gitRequest
		err  error
	}{
		{"git-upload-pack", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-upload-pack /r.git", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-receive-pack /r.git\x00host=h\x00", gitRequest{}, ErrPushNotSupported},
		{"git-x /r.git\x00host=h\x00", gitRequest{}, gitserve.ErrInvalidRequest},
		{"git-upload-pack /r.git\x00host=h\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h"}, nil},
		{"git-upload-pack /a/../r\x00host=h:9418\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h:9418"}, nil},
		{"git-upload-archive /r.git\x00\x00", gitRequest{service: gitserve.UploadArchiveService, name: "r"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=2:x\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"}, nil},
		{"git-upload-pack /r.git\x00host=h\x00\x00version=3\x00", gitRequest{service: gitserve.UploadPackService, name: "r", host: "h"}, nil},
	} {
		got, err := parseRequest([]byte(tc.line))
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("parseRequest(%q) = %+v, %v; want %+v, %v", tc.line, got, err, tc.want, tc.err)
		}
	}
}

// TestEnviron: the daemon runs no hooks, so git gets only the requested
// protocol version, never the hook variables or the server config with its
// database data source.
func TestEnviron(t *testing.T) {
	env := testDaemon.environ(gitRequest{service: gitserve.UploadPackService, name: "r", host: "h", version: "2"})
	if want := []string{"GIT_PROTOCOL=version=2"}; !slices.Equal(env, want) {
		t.Errorf("environ = %q, want %q", env, want)
	}
	if env := testDaemon.environ(gitRequest{service: gitserve.UploadPackService, name: "r"}); len(env) != 0 {
		t.Errorf("environ without a requested version = %q, want none", env)
	}
}

// TestRefusalEndsAtRequestDeadline checks that a refusal after access checks
// that outlast the request timeout does not wait on the client past the
// request deadline. A wait counted again from the refusal would let slow
// checks push it past the deadline by as long as they took.
//
// Nothing here is decided by how long a step takes. The request timeout is
// an hour, so no stall makes the request line late, and the refusal reads
// its time from a clock two request timeouts ahead, so it comes after the
// request deadline however soon it runs. Past that deadline the daemon
// lets go of the connection at once; a wait counted from the refusal would
// hold it for about the request timeout more, an hour, far past the test's
// wait.
func TestRefusalEndsAtRequestDeadline(t *testing.T) {
	t.Parallel()
	const requestTimeout = time.Hour
	d, addr := startLimitedDaemon(t, 1, func(d *Server) {
		d.requestTimeout = requestTimeout
		d.refuseLinger = time.Hour
		d.testHooks.now = func() time.Time { return time.Now().Add(2 * requestTimeout) }
	})
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(idleWait))
	if _, err := io.WriteString(c, pkt("git-upload-pack /deadline-missing.git\x00host=localhost\x00")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, err = readPktline(c)
	wantErrLine(t, "missing repo", err, gitserve.ErrNotAuthed)
	// The client neither sends nor hangs up, so only the end of the
	// refusal's wait closes the connection.
	waitConns(t, d, 0, idleWait)
}

// dialHeld connects to addr, sends request, and reads the daemon's answer,
// which must be the ERR packet want. The connection stays open, sending
// nothing more, until the test ends.
func dialHeld(t *testing.T, addr, request string, want error) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	if request != "" {
		if _, err := io.WriteString(c, request); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
	_, err = readPktline(c)
	wantErrLine(t, "answer", err, want)
	return c
}

// TestShutdownCutsRefusalWait checks that a graceful Shutdown does not wait
// for a refused client to hang up.
func TestShutdownCutsRefusalWait(t *testing.T) {
	t.Parallel()
	d, addr := startLimitedDaemon(t, 1, func(d *Server) { d.refuseLinger = time.Hour })
	dialHeld(t, addr, pkt("git-receive-pack /repo.git\x00host=localhost\x00"), ErrPushNotSupported)
	// The refusal waits on the client, which neither sends nor hangs up.
	waitConns(t, d, 1, idleWait)
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	waitConns(t, d, 0, idleWait)
}

// holdSlot dials addr and waits until d counts the connection, which then
// holds one of d's slots until it is closed or the test ends.
func holdSlot(t *testing.T, d *Server, addr string) net.Conn {
	t.Helper()
	want := d.conns.Size() + 1
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitConns(t, d, want, idleWait)
	return c
}

// TestMaxConnectionsRefusalSurvivesLateInput checks that a client turned
// away for the connection limit can keep writing, as git archive writes its
// arguments, without the daemon resetting the connection under it: a
// connection closed at once would draw a reset with the client's next
// write, and a client still writing could lose the error to it.
func TestMaxConnectionsRefusalSurvivesLateInput(t *testing.T) {
	t.Parallel()
	conns := make(chan *recordedConn, 2)
	d, addr := startDaemonOn(t, 1, func(inner net.Listener) net.Listener {
		return &recordingListener{Listener: inner, conns: conns}
	}, func(d *Server) { d.refuseLinger = time.Hour })
	accepted := func() *recordedConn {
		t.Helper()
		select {
		case c := <-conns:
			return c
		case <-time.After(idleWait):
			t.Fatal("the daemon did not accept the connection")
			return nil
		}
	}
	holdSlot(t, d, addr)
	accepted() // the held one
	c := dialHeld(t, addr, "", gitserve.ErrMaxConnections)
	refused := accepted() // accepted before it was refused
	sent := 0
	for i := range 20 {
		n, err := io.WriteString(c, pkt("argument HEAD\n"))
		if err != nil {
			t.Fatalf("write %d after the refusal: %v", i+1, err)
		}
		sent += n
	}
	refused.waitRead(t, sent)
	// The daemon shut its side after the error and its flush packet: the
	// client reads a clean end of the stream, not a reset.
	if out, err := io.ReadAll(c); err != nil || (len(out) != 0 && string(out) != "0000") {
		t.Fatalf("after the refusal: read %q, %v; want a clean end", out, err)
	}
}

// TestMaxConnectionsRefusalsDoNotBlockAccept checks that refusals that wait
// on their clients do not hold up the accept loop, also past maxRefusing of
// them, and that Shutdown does not wait on them.
func TestMaxConnectionsRefusalsDoNotBlockAccept(t *testing.T) {
	t.Parallel()
	d, addr := startLimitedDaemon(t, 1, func(d *Server) { d.refuseLinger = time.Hour })
	held := holdSlot(t, d, addr)
	for range maxRefusing + 4 {
		dialHeld(t, addr, "", gitserve.ErrMaxConnections)
	}
	// The first maxRefusing refusals still wait on their clients; the
	// rest were closed at once.
	if n := len(d.refusing); n != maxRefusing {
		t.Errorf("%d refusals in progress, want %d", n, maxRefusing)
	}
	if got := d.conns.Size(); got != 1 {
		t.Errorf("%d connections counted, want 1", got)
	}
	// Shutdown would wait for the connection in the slot, which has not
	// sent its request yet.
	_ = held.Close()
	ctx, cancel := context.WithTimeout(t.Context(), idleWait)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}

// syncBuffer is a bytes.Buffer safe for the daemon's goroutines to log to
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newRequestTestDaemon serves a daemon that logs at Info level to the
// returned buffer, on its own loopback listener.
func newRequestTestDaemon(t *testing.T, hooks testHooks) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := log.New(logs)
	logger.SetLevel(log.InfoLevel)
	d, err := NewServer(testDaemon.cfg, testBackend, logger)
	if err != nil {
		t.Fatal(err)
	}
	patient(d)
	d.testHooks = hooks
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.addr = l.Addr().String()
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })
	return d, logs
}

// request asks d for repo with service and returns the first line it
// answers with, after the connection is closed.
func request(t *testing.T, d *Server, service gitserve.Service, repo string) string {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", d.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if _, err := io.WriteString(c, pkt(service.String()+" /"+repo+".git\x00host=localhost\x00")); err != nil {
		t.Fatalf("failed to send request: %v", err)
	}
	line, err := readPktline(c)
	if err != nil {
		return err.Error()
	}
	return line
}

// sampleCount returns how many observations the request duration histogram
// holds for service.
func sampleCount(t *testing.T, service gitserve.Service) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(requestSeconds)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "service" && lp.GetValue() == service.String() {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// The access checks run under accessTimeout: a database lookup that runs
// out of time refuses the request instead of waiting on the database.
func TestReadableAccessTimeout(t *testing.T) {
	if _, err := testBackend.CreateRepository(context.Background(), "timeout-public", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create repo: %v", err)
	}
	d, err := NewServer(testDaemon.cfg, testBackend, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.accessTimeout != defaultAccessTimeout {
		t.Fatalf("accessTimeout = %v, want %v", d.accessTimeout, defaultAccessTimeout)
	}
	// The lookup that must succeed gets as long as a slowed database may
	// take.
	d.accessTimeout = patience
	if ok, err := d.readable(context.Background(), "timeout-public"); !ok || err != nil {
		t.Fatalf("public repository is not readable: %v", err)
	}
	// A deadline already in the past expires before any query runs. A tiny
	// positive timeout is not enough: on Windows the timer can fire after the
	// fast checks have finished.
	d.accessTimeout = -time.Second
	if ok, _ := d.readable(context.Background(), "timeout-public"); ok {
		t.Fatal("public repository is readable although the access checks ran out of time")
	}
}

// An accepted request is logged at Info level and timed; a refused one is
// neither.
func TestAcceptedRequestLoggedAndTimed(t *testing.T) {
	if _, err := testBackend.CreateRepository(context.Background(), "accepted-public", fixtureAdmin(t, testBackend), proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
		t.Fatalf("failed to create repo: %v", err)
	}
	var timed atomic.Int32
	d, logs := newRequestTestDaemon(t, testHooks{requestTimed: func(service gitserve.Service) {
		if service == gitserve.UploadPackService {
			timed.Add(1)
		} else {
			t.Errorf("timed a %s request; this test sends only upload-pack", service)
		}
	}})

	before := sampleCount(t, gitserve.UploadPackService)
	if got := request(t, d, gitserve.UploadPackService, "accepted-missing"); got != gitserve.ErrNotAuthed.Error() {
		t.Fatalf("missing repo: got %q, want %q", got, gitserve.ErrNotAuthed)
	}
	if got := request(t, d, gitserve.UploadPackService, "accepted-public"); got == gitserve.ErrNotAuthed.Error() {
		t.Fatalf("public repo was refused: %q", got)
	}

	// The daemon records a request, and logs what it logs about it, before
	// it lets go of its connection, which it does after the client hangs
	// up: once it holds no connection, it is done with both requests.
	waitConns(t, d, 0, idleWait)
	// This daemon timed the accepted request once, and the refused one not
	// at all.
	if got := timed.Load(); got != 1 {
		t.Errorf("upload-pack requests this daemon timed = %d, want 1", got)
	}
	// The observation went to the histogram the metrics endpoint serves.
	// Other tests' daemons observe into it too: Close does not wait for
	// their requests to end, so one may finish meanwhile, and the global
	// count can only be bounded from below.
	if got := sampleCount(t, gitserve.UploadPackService) - before; got < 1 {
		t.Errorf("request duration observations = %d, want at least 1", got)
	}

	out := logs.String()
	if !strings.Contains(out, "accepted request") || !strings.Contains(out, "repo=accepted-public") {
		t.Errorf("accepted request not logged at Info level; logs:\n%s", out)
	}
	if strings.Contains(out, "accepted-missing") {
		t.Errorf("refused request logged as accepted; logs:\n%s", out)
	}
}
