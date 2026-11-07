package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

// download GETs oid from the basic download route, adding the given headers.
func (e *lfsUploadEnv) download(oid string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(e.ctx, http.MethodGet,
		"/repo.git/info/lfs/objects/basic/"+oid, nil)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "token "+e.token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// storeObject writes content straight to the object path without recording
// it in the database, as for an object whose row the batch API has not
// created yet.
func (e *lfsUploadEnv) storeObject(t *testing.T, content string) string {
	t.Helper()
	oid := sha256Hex(content)
	p := e.objectPath(oid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return oid
}

// TestLFSDownloadWithoutDatabaseRow checks that the basic download handler
// serves a stored object whose database row is missing. It used to take the
// length from the missing row and send Content-Length: 0 followed by the
// object, which failed the copy.
func TestLFSDownloadWithoutDatabaseRow(t *testing.T) {
	t.Parallel()
	const content = "hello lfs download"
	e := newLFSUploadEnv(t)
	oid := e.storeObject(t, content)
	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid); err == nil {
		t.Fatal("test setup: object unexpectedly has a database row")
	}

	w := e.download(oid, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200: %s", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Content-Length"), strconv.Itoa(len(content)); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("body = %q, want %q", got, content)
	}
}

// TestLFSDownloadRange checks that a download can resume with a Range
// request.
func TestLFSDownloadRange(t *testing.T) {
	t.Parallel()
	const content = "0123456789abcdef"
	e := newLFSUploadEnv(t)
	oid := e.storeObject(t, content)

	w := e.download(oid, map[string]string{"Range": "bytes=10-"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("got status %d, want 206: %s", w.Code, w.Body.String())
	}
	if got, want := w.Body.String(), content[10:]; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Content-Range"), "bytes 10-15/16"; got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}

// TestLFSDownloadMissingObject checks that a download of an object that is
// not stored answers 404.
func TestLFSDownloadMissingObject(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	w := e.download(sha256Hex("not stored"), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want 404: %s", w.Code, w.Body.String())
	}
}
