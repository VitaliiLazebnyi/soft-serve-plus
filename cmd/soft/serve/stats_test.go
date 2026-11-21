package serve

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func getStats(t *testing.T, ctx context.Context, url string) (int, string) {
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
	ctx := context.Background()
	s := newStatsServer()

	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()

	base := "http://" + l.Addr().String()

	code, body := getStats(t, ctx, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", code, http.StatusOK)
	}
	for _, want := range []string{"go_goroutines", "process_start_time_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /metrics body has no %q", want)
		}
	}

	if code, _ := getStats(t, ctx, base+"/"); code != http.StatusNotFound {
		t.Errorf("GET / = %d, want %d", code, http.StatusNotFound)
	}

	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve() after Shutdown = %v, want %v", err, http.ErrServerClosed)
	}
}
