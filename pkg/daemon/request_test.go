package daemon

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/prometheus/client_golang/prometheus"
)

type ctxKey struct{}

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
func newRequestTestDaemon(t *testing.T, ctx context.Context) (*GitDaemon, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := log.New(logs)
	logger.SetLevel(log.InfoLevel)
	ctx = config.WithContext(ctx, testDaemon.cfg)
	ctx = backend.WithContext(ctx, testBackend)
	ctx = log.WithContext(ctx, logger)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
func request(t *testing.T, d *GitDaemon, service git.Service, repo string) string {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", d.addr)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := pktline.NewEncoder(c).EncodeString(service.String() + " /" + repo + ".git\x00host=localhost\x00"); err != nil {
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
func sampleCount(t *testing.T, service git.Service) uint64 {
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

// The request context derives from the server's: it carries the server's
// values and ends when the server's context does.
func TestRequestContextDerivesFromServer(t *testing.T) {
	sctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "server"))
	d := &GitDaemon{ctx: sctx}
	ctx, rcancel := d.requestContext()
	defer rcancel()
	if got := ctx.Value(ctxKey{}); got != "server" {
		t.Fatalf("request context value = %v, want the server's", got)
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("request context outlived the server's")
	}
}

// The access checks run under accessTimeout: a database lookup that runs
// out of time refuses the request instead of waiting on the database.
func TestReadableAccessTimeout(t *testing.T) {
	if _, err := testBackend.CreateRepository(context.Background(), "timeout-public", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	ctx := config.WithContext(context.Background(), testDaemon.cfg)
	ctx = backend.WithContext(ctx, testBackend)
	d, err := NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.accessTimeout != defaultAccessTimeout {
		t.Fatalf("accessTimeout = %v, want %v", d.accessTimeout, defaultAccessTimeout)
	}
	if !d.readable(context.Background(), "timeout-public") {
		t.Fatal("public repository is not readable")
	}
	// A deadline already in the past expires before any query runs. A tiny
	// positive timeout is not enough: on Windows the timer can fire after the
	// (fast, in-memory) checks have finished.
	d.accessTimeout = -time.Second
	if d.readable(context.Background(), "timeout-public") {
		t.Fatal("public repository is readable although the access checks ran out of time")
	}
}

// An accepted request is logged at Info level and timed; a refused one is
// neither.
func TestAcceptedRequestLoggedAndTimed(t *testing.T) {
	if _, err := testBackend.CreateRepository(context.Background(), "accepted-public", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	d, logs := newRequestTestDaemon(t, context.Background())

	before := sampleCount(t, git.UploadPackService)
	if got := request(t, d, git.UploadPackService, "accepted-missing"); got != git.ErrNotAuthed.Error() {
		t.Fatalf("missing repo: got %q, want %q", got, git.ErrNotAuthed)
	}
	if got := request(t, d, git.UploadPackService, "accepted-public"); got == git.ErrNotAuthed.Error() {
		t.Fatalf("public repo was refused: %q", got)
	}

	// The daemon records the request after the client hangs up.
	deadline := time.Now().Add(5 * time.Second)
	for sampleCount(t, git.UploadPackService) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Other tests' requests may finish meanwhile, so only check that this
	// one was counted.
	if got := sampleCount(t, git.UploadPackService) - before; got < 1 {
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
