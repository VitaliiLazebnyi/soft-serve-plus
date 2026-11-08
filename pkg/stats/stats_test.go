package stats

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
)

func get(t *testing.T, ctx context.Context, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// The stats server serves the Prometheus metrics on /metrics, and nothing
// else, and stops on Shutdown.
func TestStatsServer(t *testing.T) {
	cfg := config.DefaultConfig()
	ctx := config.WithContext(context.Background(), cfg)
	s, err := NewStatsServer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()

	base := "http://" + l.Addr().String()

	code, body := get(t, ctx, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", code, http.StatusOK)
	}
	for _, want := range []string{"go_goroutines", "process_start_time_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /metrics body has no %q", want)
		}
	}

	if code, _ := get(t, ctx, base+"/"); code != http.StatusNotFound {
		t.Errorf("GET / = %d, want %d", code, http.StatusNotFound)
	}

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve() after Shutdown = %v, want %v", err, http.ErrServerClosed)
	}
}

// The server listens on the configured stats address.
func TestStatsServerAddr(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Stats.ListenAddr = "127.0.0.1:12345"
	s, err := NewStatsServer(config.WithContext(context.Background(), cfg))
	if err != nil {
		t.Fatal(err)
	}
	if s.server.Addr != cfg.Stats.ListenAddr {
		t.Errorf("server address = %q, want %q", s.server.Addr, cfg.Stats.ListenAddr)
	}
}
