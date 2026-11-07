package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/gorilla/mux"
)

// trackingBody records whether a read of the request body is in progress.
type trackingBody struct {
	io.ReadCloser
	reading atomic.Bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.reading.Store(true)
	defer b.reading.Store(false)
	return b.ReadCloser.Read(p)
}

// A client that sends a gzip request git rejects, so git exits, and then
// keeps the request open without sending more must not leave the body being
// read after the handler returns, or the deferred close of the gzip reader
// racing that read.
func TestServiceRpcJoinsBodyRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()

	returned := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &trackingBody{ReadCloser: r.Body}
		r.Body = body
		r = r.WithContext(config.WithContext(r.Context(), cfg))
		r = mux.SetURLVars(r, map[string]string{
			"service": "git-upload-pack",
			"dir":     dir,
			"repo":    "repo",
		})
		serviceRpc(w, r)
		returned <- body.reading.Load()
	}))
	defer srv.Close()

	pr, pw := io.Pipe()
	defer pw.Close() //nolint: errcheck
	go func() {
		zw := gzip.NewWriter(pw)
		// Not a pkt-line: upload-pack exits on it.
		zw.Write([]byte("zzzz")) //nolint: errcheck
		zw.Flush()               //nolint: errcheck
		// The request stays open until the test ends.
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/repo.git/git-upload-pack", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	go func() {
		resp, err := srv.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body) //nolint: errcheck
			resp.Body.Close()              //nolint: errcheck
		}
	}()

	select {
	case reading := <-returned:
		if reading {
			t.Error("the handler returned while the request body was still being read")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the handler to return")
	}
}
