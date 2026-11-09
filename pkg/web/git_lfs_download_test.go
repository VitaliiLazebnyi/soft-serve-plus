package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
)

// anonymousBatch posts a download batch for one object without credentials.
func (e *lfsUploadEnv) anonymousBatch(t *testing.T, oid string, size int64) lfs.ObjectResponse {
	t.Helper()
	body := fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":%d}]}`, oid, size)
	req := httptest.NewRequestWithContext(e.ctx, http.MethodPost,
		"/repo.git/info/lfs/objects/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", lfs.MediaType)
	req.Header.Set("Accept", lfs.MediaType)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("batch: got status %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp lfs.BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Objects) != 1 {
		t.Fatalf("batch: got %d objects, want 1", len(resp.Objects))
	}
	return *resp.Objects[0]
}

// TestLFSDownloadBatchDoesNotRecordObjects checks that a download batch for
// an object that is on disk but not recorded (an upload between storing and
// recording it) records nothing. It used to record the size the client
// sent, so an anonymous reader could pin a wrong size: later downloads got
// "size mismatch" and uploads of the object were skipped.
func TestLFSDownloadBatchDoesNotRecordObjects(t *testing.T) {
	e := newLFSUploadEnv(t)
	const content = "hello lfs"
	oid := sha256Hex(content)
	size := int64(len(content))
	if err := os.MkdirAll(filepath.Dir(e.objectPath(oid)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.objectPath(oid), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	assertNotRecorded := func() {
		t.Helper()
		_, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid)
		if !errors.Is(err, db.ErrRecordNotFound) {
			t.Fatalf("object recorded by a download batch (err %v)", err)
		}
	}

	for _, wrong := range []int64{0, size + 1} {
		obj := e.anonymousBatch(t, oid, wrong)
		if obj.Error == nil || obj.Error.Code != http.StatusUnprocessableEntity {
			t.Errorf("size %d: got %+v, want a 422 error", wrong, obj)
		}
		assertNotRecorded()
	}

	obj := e.anonymousBatch(t, oid, size)
	if obj.Error != nil || obj.Actions[lfs.ActionDownload] == nil {
		t.Fatalf("got %+v, want a download action", obj)
	}
	assertNotRecorded()

	// The object downloads at its size on disk.
	req := httptest.NewRequestWithContext(e.ctx, http.MethodGet,
		"/repo.git/info/lfs/objects/basic/"+oid, nil)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download: got status %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Length"); got != strconv.FormatInt(size, 10) {
		t.Errorf("download: Content-Length %q, want %d", got, size)
	}
	got, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("download: got %q, want %q", got, content)
	}

	// An upload still stores and records it.
	if w := e.upload(oid, size, strings.NewReader(content)); w.Code != http.StatusOK {
		t.Fatalf("upload: got status %d, want 200: %s", w.Code, w.Body.String())
	}
	rec, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Size != size {
		t.Errorf("recorded size %d, want %d", rec.Size, size)
	}
}
