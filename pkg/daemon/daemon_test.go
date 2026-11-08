package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
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
		{"invalid pkt-len", []byte("zzzzgit-upload-pack /repo.git\x00"), git.ErrInvalidRequest},
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
