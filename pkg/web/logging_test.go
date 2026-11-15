package web

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
)

// TestLoggingMiddlewareServeContent serves a file through the logging
// middleware on a real server and checks that full and ranged responses
// arrive intact and are counted in the response log.
func TestLoggingMiddlewareServeContent(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 10000)
	name := filepath.Join(t.TempDir(), "obj")
	if err := os.WriteFile(name, content, 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	h := newLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(name)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close() //nolint: errcheck
		http.ServeContent(w, r, "", time.Time{}, f)
	}), logger)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint: errcheck
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("GET: status %d, %d bytes; want 200, %d bytes", resp.StatusCode, len(body), len(content))
	}

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=50000-")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close() //nolint: errcheck
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[50000:]) {
		t.Fatalf("ranged GET: status %d, %d bytes; want 206, %d bytes", resp.StatusCode, len(body), len(content)-50000)
	}

	// Close waits for the handlers to return, so both "response" lines
	// have been written before the log buffer is read.
	srv.Close()
	out := logs.String()
	if !strings.Contains(out, `bytes="100 kB"`) || !strings.Contains(out, `bytes="50 kB"`) {
		t.Fatalf("response log does not count the served bytes:\n%s", out)
	}
}
