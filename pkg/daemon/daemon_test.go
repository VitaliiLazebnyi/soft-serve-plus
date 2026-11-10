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
	"runtime"
	"runtime/pprof"
	"strings"
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
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
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
	cfg.Git.IdleTimeout = 1
	cfg.Git.ListenAddr = fmt.Sprintf("127.0.0.1:%d", test.RandomPort()) // RandomPort checks only loopback
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	if err != nil {
		log.Fatal(err)
	}
	defer dbx.Close() //nolint: errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		log.Fatal(err)
	}
	datastore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, datastore)
	be := backend.New(ctx, cfg, dbx, datastore)
	testBackend = be
	ctx = backend.WithContext(ctx, be)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		log.Fatal(err)
	}
	testDaemon = d
	defer d.Close()       //nolint:errcheck
	go d.ListenAndServe() //nolint:errcheck
	return m.Run()
}

func TestIdleTimeout(t *testing.T) {
	var err error
	var c net.Conn
	var tries int
	var dialer net.Dialer
	for {
		c, err = dialer.DialContext(t.Context(), "tcp", testDaemon.addr)
		if err != nil && tries >= 3 {
			t.Fatalf("failed to connect to daemon after %d tries: %v", tries, err)
		}
		tries++
		if testDaemon.conns.Size() != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	_, err = readPktline(c)
	if err == nil || err.Error() != git.ErrTimeout.Error() {
		t.Errorf("expected %q, got %v", git.ErrTimeout, err)
	}
}

// TestBadRequests verifies the daemon answers malformed requests and pushes
// with a reply that says what is wrong.
func TestBadRequests(t *testing.T) {
	pkt := func(s string) []byte {
		var b bytes.Buffer
		if err := pktline.NewEncoder(&b).EncodeString(s); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		// Only the bad length prefix is sent: bytes the daemon never reads
		// would make Windows reset the connection when the daemon closes it,
		// discarding the reply before the client reads it.
		{"invalid pkt-len", []byte("zzzz"), git.ErrInvalidRequest},
		{"client error packet", pkt("ERR boom\n"), git.ErrInvalidRequest},
		{"unknown service", pkt("git-frobnicate /repo.git\x00"), git.ErrInvalidRequest},
		{"push", pkt("git-receive-pack /repo.git\x00host=localhost\x00"), ErrPushNotSupported},
	}
	for _, tc := range cases {
		if got := rawRequest(t, tc.raw); got != tc.want.Error() {
			t.Errorf("%s: expected %q, got %q", tc.name, tc.want, got)
		}
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
	var dialer net.Dialer
	dial := func() *net.TCPConn {
		t.Helper()
		c, err := dialer.DialContext(t.Context(), "tcp", testDaemon.addr)
		if err != nil {
			t.Fatalf("failed to connect to daemon: %v", err)
		}
		return c.(*net.TCPConn)
	}

	// Idle until the daemon gives up on the request.
	idle := dial()
	defer idle.Close() //nolint:errcheck
	_ = idle.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := readPktline(idle); err == nil || !strings.Contains(err.Error(), git.ErrTimeout.Error()) {
		t.Errorf("idle connection: expected %q, got %v", git.ErrTimeout, err)
	}

	// Reset the connection before sending anything.
	for range 5 {
		c := dial()
		_ = c.SetLinger(0)
		_ = c.Close()
	}

	deadline := time.Now().Add(10 * time.Second)
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
// line it answers with.
func requestUploadPack(t *testing.T, repo string) string {
	t.Helper()
	var b bytes.Buffer
	if err := pktline.NewEncoder(&b).EncodeString("git-upload-pack /" + repo + ".git\x00"); err != nil {
		t.Fatalf("expected nil, got error: %v", err)
	}
	return rawRequest(t, b.Bytes())
}

// rawRequest sends raw bytes to the daemon and returns the first line it
// answers with.
func rawRequest(t *testing.T, raw []byte) string {
	t.Helper()
	c, err := net.Dial("tcp", testDaemon.addr) //nolint:noctx
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if _, err := c.Write(raw); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	// The scanner reports an ERR packet as an error carrying its message.
	line, err := readPktline(c)
	if err != nil {
		return err.Error()
	}
	return line
}

// TestInvalidRepo verifies the daemon, which is always anonymous, answers a
// missing repository and a private one identically so neither is revealed.
func TestInvalidRepo(t *testing.T) {
	ctx := context.Background()
	if _, err := testBackend.CreateRepository(ctx, "private", nil, proto.RepositoryOptions{Private: true}); err != nil {
		t.Fatalf("failed to create private repo: %v", err)
	}

	if _, err := testBackend.CreateRepository(ctx, "public", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create public repo: %v", err)
	}

	want := git.ErrNotAuthed.Error()
	missing := requestUploadPack(t, "test")
	private := requestUploadPack(t, "private")
	if missing != want {
		t.Errorf("missing repo: expected %q, got %q", want, missing)
	}
	if private != want {
		t.Errorf("private repo: expected %q, got %q", want, private)
	}

	// A path that escapes the repositories directory gets the same answer.
	if got := requestUploadPack(t, "../escape"); got != want {
		t.Errorf("escaping path: expected %q, got %q", want, got)
	}

	// A public repository is served: the first line is not a refusal.
	public := requestUploadPack(t, "public")
	if public == want || public == git.ErrInvalidRepo.Error() {
		t.Errorf("public repo was refused: %q", public)
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
	if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
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
	if _, err := testBackend.CreateRepository(t.Context(), "stderr", nil, proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
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
	sc := pktline.NewScanner(bytes.NewReader(out))
	for sc.Scan() { //nolint:revive
	}
	if err := sc.Err(); err != nil {
		t.Errorf("answer is not a valid pkt-line stream: %v: %q", err, out)
	}
}

// TestGitFailureFixedMessage verifies that when git fails before answering,
// the client gets a fixed error message instead of git's output and error.
func TestGitFailureFixedMessage(t *testing.T) {
	if _, err := testBackend.CreateRepository(t.Context(), "broken", nil, proto.RepositoryOptions{}); err != nil && !errors.Is(err, proto.ErrRepoExist) {
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

func readPktline(c net.Conn) (string, error) {
	pktout := pktline.NewScanner(c)
	if !pktout.Scan() {
		return "", pktout.Err()
	}
	return strings.TrimSpace(string(pktout.Bytes())), nil
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

		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		case <-time.After(5 * time.Second):
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

// startLimitedDaemon serves a daemon allowing limit connections that never
// time out, and returns it with its address.
func startLimitedDaemon(t *testing.T, limit int) (*GitDaemon, string) {
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
	d.requestTimeout = time.Hour
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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

// refused dials addr and reports whether the daemon turns the connection away
// as over the limit. A connection it keeps open sends nothing, so it counts as
// kept once wait passes without an answer.
func refused(t *testing.T, addr string, wait time.Duration) (bool, net.Conn) {
	t.Helper()
	var dialer net.Dialer
	c, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(wait))
	_, err = readPktline(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, c
	}
	if err == nil || err.Error() != git.ErrMaxConnections.Error() {
		t.Fatalf("got %v, want %q or no answer", err, git.ErrMaxConnections)
	}
	return true, c
}

// TestMaxConnections checks that max_connections N lets N connections in and
// refuses the next. It used to let only N-1 in, and a limit of 1 none.
func TestMaxConnections(t *testing.T) {
	for _, limit := range []int{1, 3} {
		d, addr := startLimitedDaemon(t, limit)
		for i := range limit {
			if r, _ := refused(t, addr, 200*time.Millisecond); r {
				t.Fatalf("limit %d: connection %d refused", limit, i+1)
			}
		}
		if r, _ := refused(t, addr, 5*time.Second); !r {
			t.Fatalf("limit %d: connection %d kept, want it refused", limit, limit+1)
		}
		if got := d.conns.Size(); got != limit {
			t.Fatalf("limit %d: %d connections open", limit, got)
		}
	}
}

// TestMaxConnectionsBurst dials many connections at once: exactly limit of
// them are kept. Connections used to be counted in their own goroutines, so
// a burst passed the check before any was counted.
func TestMaxConnectionsBurst(t *testing.T) {
	const limit, dials = 3, 64
	d, addr := startLimitedDaemon(t, limit)
	// Each refused connection gets an answer; wait long enough for all of
	// them before counting the silent ones as kept.
	type result struct {
		c       net.Conn
		err     error
		refused bool
	}
	results := make(chan result, dials)
	for range dials {
		go func() {
			var dialer net.Dialer
			c, err := dialer.DialContext(t.Context(), "tcp", addr)
			if err != nil {
				results <- result{err: err}
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err = readPktline(c)
			results <- result{c: c, refused: err != nil && err.Error() == git.ErrMaxConnections.Error()}
		}()
	}
	// A burst of loopback dials can time out on macOS when the listen
	// backlog drops SYNs, without the daemon being involved. Such a dial
	// never reaches the daemon, so skip it and check the limit over the
	// connections that did get through.
	var kept, connected int
	for range dials {
		r := <-results
		if r.err != nil {
			t.Logf("dial: %v", r.err)
			continue
		}
		connected++
		t.Cleanup(func() { _ = r.c.Close() })
		if !r.refused {
			kept++
		}
	}
	if connected <= limit {
		t.Skipf("only %d of %d dials connected, need more than %d", connected, dials, limit)
	}
	if kept != limit {
		t.Errorf("%d of %d connections kept, want %d", kept, connected, limit)
	}
	if got := d.conns.Size(); got != limit {
		t.Errorf("%d connections open, want %d", got, limit)
	}
}

// TestConnectionsAddAfterCloseAll checks that a connection accepted while the
// daemon closes is not added after CloseAll and so escapes the forced close.
func TestConnectionsAddAfterCloseAll(t *testing.T) {
	m := connections{m: make(map[net.Conn]struct{})}
	a, b := net.Pipe()
	defer a.Close() //nolint:errcheck
	defer b.Close() //nolint:errcheck
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(a, 10); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Add after CloseAll = %v, want ErrServerClosed", err)
	}
	if got := m.Size(); got != 0 {
		t.Fatalf("%d connections after CloseAll, want 0", got)
	}
}

// TestRequestTimeout checks that a client trickling its request line in a
// byte at a time, each well within the idle timeout, is still dropped once
// the request timeout passes, and is told why. Every read used to push the
// deadline forward, so such clients could hold a connection slot
// indefinitely.
func TestRequestTimeout(t *testing.T) {
	const (
		requestTimeout = 2 * time.Second
		idleTimeout    = 5 * time.Second
	)
	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = int(idleTimeout / time.Second)
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

	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	// Start a request line whose declared length is more than is ever
	// sent, a byte every 200ms for the first half of the request timeout.
	// The client then stops writing well before the request deadline, so
	// its writes cannot race the daemon closing the connection and turn
	// the daemon's answer into a reset.
	start := time.Now()
	for _, b := range []byte("0fffgit") {
		if time.Since(start) > requestTimeout/2 {
			break
		}
		if _, err := c.Write([]byte{b}); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := c.SetReadDeadline(time.Now().Add(2 * idleTimeout)); err != nil {
		t.Fatal(err)
	}
	// The scanner reports an ERR packet as an error carrying its message.
	line, err := readPktline(c)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a timeout error, got %q", line)
	}
	if !strings.Contains(err.Error(), git.ErrTimeout.Error()) {
		t.Fatalf("expected the daemon to report %q, got %v", git.ErrTimeout, err)
	}
	// The idle deadline after the last byte would only pass at about
	// requestTimeout/2 + idleTimeout.
	if elapsed > requestTimeout+idleTimeout/2 {
		t.Fatalf("connection was dropped only after %v, want about %v", elapsed, requestTimeout)
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
	if _, err := testBackend.CreateRepository(ctx, name, nil, proto.RepositoryOptions{}); err != nil {
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
func TestQuietCloneSlowPack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pack-objects hook needs a POSIX shell")
	}
	ctx := context.Background()
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

	cfg := *testDaemon.cfg
	cfg.Git.IdleTimeout = 2
	dctx := config.WithContext(context.Background(), &cfg)
	dctx = backend.WithContext(dctx, testBackend)
	d, err := NewGitDaemon(dctx)
	if err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve(l) //nolint:errcheck
	t.Cleanup(func() { _ = d.Close() })

	// Delay the pack past the idle timeout. The hook is only honoured
	// from protected configuration, which the command-line scope of
	// GIT_CONFIG_COUNT is; the daemon passes its environment to git.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "uploadpack.packObjectsHook")
	t.Setenv("GIT_CONFIG_VALUE_0", "sleep 3;")

	dst := filepath.Join(t.TempDir(), "clone")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "-q", "git://"+l.Addr().String()+"/"+name, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "README")); err != nil {
		t.Errorf("clone is missing README: %v", err)
	}
}

// TestNoErrAfterOutput leaves a request idle after upload-pack answered with
// its references. The daemon closes the connection, but must not write an
// ERR packet: the client already reads the response, so it would take the
// packet for part of it.
func TestNoErrAfterOutput(t *testing.T) {
	const name = "idleafteradvert"
	ensureRepo(t, name)
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", testDaemon.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := pktline.NewEncoder(c).EncodeString("git-upload-pack /" + name + ".git\x00"); err != nil {
		t.Fatal(err)
	}
	// Stay silent until the daemon gives up on the connection, then
	// collect everything it sent.
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected a response, got nothing")
	}
	if strings.Contains(string(out), "ERR ") {
		t.Errorf("response has an ERR packet after output started:\n%q", out)
	}
}
