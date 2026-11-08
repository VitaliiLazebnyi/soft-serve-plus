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

// readerFromRecorder is a ResponseWriter whose ReadFrom records that it was
// used, standing in for net/http's response, which sends files with
// sendfile(2) from ReadFrom.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalls int
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalls++
	return io.Copy(r.ResponseRecorder, src)
}

func TestLogWriterReadFromForwards(t *testing.T) {
	under := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	lw := &logWriter{ResponseWriter: under, code: http.StatusOK}

	// Hide strings.Reader's WriteTo so io.Copy goes through ReadFrom, as it
	// does for a file.
	n, err := io.Copy(lw, struct{ io.Reader }{strings.NewReader("hello world")})
	if err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if n != 11 {
		t.Fatalf("copied %d bytes, want 11", n)
	}
	if under.readFromCalls != 1 {
		t.Fatalf("underlying ReadFrom called %d times, want 1", under.readFromCalls)
	}
	if lw.bytes != 11 {
		t.Fatalf("logged %d bytes, want 11", lw.bytes)
	}
	if got := under.Body.String(); got != "hello world" {
		t.Fatalf("body %q, want %q", got, "hello world")
	}
}

// plainWriter is a ResponseWriter without ReadFrom.
type plainWriter struct {
	http.ResponseWriter
}

func TestLogWriterReadFromFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	lw := &logWriter{ResponseWriter: plainWriter{rec}, code: http.StatusOK}

	n, err := lw.ReadFrom(strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != 3 || lw.bytes != 3 {
		t.Fatalf("ReadFrom = %d, logged %d; want 3, 3", n, lw.bytes)
	}
	if got := rec.Body.String(); got != "abc" {
		t.Fatalf("body %q, want %q", got, "abc")
	}
}

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

	h := NewLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("logging middleware writer does not implement io.ReaderFrom")
		}
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
