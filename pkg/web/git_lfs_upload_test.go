package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

type lfsUploadEnv struct {
	env       *testEnv
	ctx       context.Context
	datastore store.Store
	repo      proto.Repository
	token     string
	router    http.Handler
	lfsDir    string
}

// newLFSUploadEnv returns an env whose server logs on log.Default().
func newLFSUploadEnv(t *testing.T) *lfsUploadEnv {
	t.Helper()
	return newLFSUploadEnvLogging(t, nil)
}

// newLFSUploadEnvLogging returns an env whose backend and server log on
// logger.
func newLFSUploadEnvLogging(t *testing.T, logger *log.Logger) *lfsUploadEnv {
	t.Helper()
	env := newTestEnvLogging(t, logger)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, owner, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := env.cfg
	return &lfsUploadEnv{
		env:       env,
		ctx:       ctx,
		datastore: datastore,
		repo:      repo,
		token:     token,
		router:    env.gitRouter(t),
		lfsDir:    filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10)),
	}
}

// upload PUTs body to the basic upload route for oid, declaring size as the
// content length.
func (e *lfsUploadEnv) upload(oid string, size int64, body io.Reader) *httptest.ResponseRecorder {
	return e.uploadWithToken(e.token, oid, size, body)
}

// uploadWithToken is upload, authenticated with token.
func (e *lfsUploadEnv) uploadWithToken(token, oid string, size int64, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(e.ctx, http.MethodPut,
		"/repo.git/info/lfs/objects/basic/"+oid, body)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	req.ContentLength = size
	req.Header.Set("Authorization", "token "+token)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *lfsUploadEnv) objectPath(oid string) string {
	return filepath.Join(e.lfsDir, "objects", oid[0:2], oid[2:4], oid)
}

// assertNoTempFiles fails if an upload left anything in the incomplete
// directory.
func (e *lfsUploadEnv) assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.lfsDir, "incomplete"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		t.Errorf("temp file left behind: %s", ent.Name())
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestLFSUploadRejectsMismatchedContent checks that the basic upload handler
// verifies the body against the oid and the declared size. It used to write
// the body straight to the object path and record it, so a client could
// store arbitrary content under any oid.
func TestLFSUploadRejectsMismatchedContent(t *testing.T) {
	t.Parallel()
	const content = "hello lfs"
	oid := sha256Hex(content)
	cases := []struct {
		name string
		oid  string
		size int64
		body string
	}{
		{"wrong hash", sha256Hex("something else"), int64(len(content)), content},
		{"body shorter than size", oid, int64(len(content)) + 1, content},
		{"body longer than size", oid, int64(len(content)) - 1, content},
		{"truncated body", oid, int64(len(content)), content[:len(content)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newLFSUploadEnv(t)
			w := e.upload(tc.oid, tc.size, strings.NewReader(tc.body))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got status %d, want 422: %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(e.objectPath(tc.oid)); !os.IsNotExist(err) {
				t.Errorf("object stored despite mismatch (stat err: %v)", err)
			}
			if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), tc.oid); err == nil {
				t.Error("object recorded despite mismatch")
			}
			e.assertNoTempFiles(t)
		})
	}
}

// TestLFSUploadRequiresContentLength checks an upload whose length is unknown,
// such as a chunked body. net/http drops the Content-Length header then, so
// the handler must not rely on the header: it answers 411, since the object
// cannot be checked against a declared size, and stores nothing.
func TestLFSUploadRequiresContentLength(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	const content = "hello lfs"
	oid := sha256Hex(content)

	req := httptest.NewRequestWithContext(e.ctx, http.MethodPut,
		"/repo.git/info/lfs/objects/basic/"+oid, strings.NewReader(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "token "+e.token)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)

	if w.Code != http.StatusLengthRequired {
		t.Fatalf("got status %d, want 411: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, lfs.MediaType) {
		t.Errorf("Content-Type = %q, want an LFS JSON error", ct)
	}
	if _, err := os.Stat(e.objectPath(oid)); !os.IsNotExist(err) {
		t.Errorf("object stored without a content length (stat err: %v)", err)
	}
	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid); err == nil {
		t.Error("object recorded without a content length")
	}
	e.assertNoTempFiles(t)
}

// TestLFSUploadStoresVerifiedObject checks the happy path, including a
// repeated upload of an object that is already stored.
func TestLFSUploadStoresVerifiedObject(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	const content = "hello lfs"
	oid := sha256Hex(content)

	for i := range 2 {
		w := e.upload(oid, int64(len(content)), strings.NewReader(content))
		if w.Code != http.StatusOK {
			t.Fatalf("upload %d: got status %d, want 200: %s", i, w.Code, w.Body.String())
		}
	}

	got, err := os.ReadFile(e.objectPath(oid))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("stored content %q, want %q", got, content)
	}
	obj, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("recorded size %d, want %d", obj.Size, len(content))
	}
	e.assertNoTempFiles(t)
}

// hookReader runs hook once, on the first Read, before reading from r. The
// handler only reads the body after its "already stored?" check, so the hook
// runs between that check and the database insert.
type hookReader struct {
	r    io.Reader
	once sync.Once
	hook func()
}

func (h *hookReader) Read(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.r.Read(p)
}

// uploadFence holds uploads at their first body read until each of n uploads
// has either reached that read or returned. An upload that fails before
// reading its body, such as on a database error, still counts, so it cannot
// leave the others waiting forever.
type uploadFence struct {
	mu      sync.Mutex
	pending int
	release chan struct{}
}

func newUploadFence(n int) *uploadFence {
	return &uploadFence{pending: n, release: make(chan struct{})}
}

// done records that one upload reached the fence or returned.
func (f *uploadFence) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending--
	if f.pending == 0 {
		close(f.release)
	}
}

// concurrentUpload is the outcome of one upload of uploadConcurrently.
type concurrentUpload struct {
	w *httptest.ResponseRecorder
	// readBody says whether the handler read the body, that is, got past
	// the existence check.
	readBody bool
}

// uploadConcurrently uploads content once per token, concurrently. Every
// upload that reads its body waits at its first read until each upload has
// read its body or returned, so uploads that get past the existence check
// all do so before any of them stores the object.
func (e *lfsUploadEnv) uploadConcurrently(content string, tokens ...string) []concurrentUpload {
	oid := sha256Hex(content)
	fence := newUploadFence(len(tokens))
	results := make([]concurrentUpload, len(tokens))
	var wg sync.WaitGroup
	for i, token := range tokens {
		wg.Go(func() {
			arrived := sync.OnceFunc(fence.done)
			// Count this upload even if it returns without reading its body.
			defer arrived()
			body := &hookReader{r: strings.NewReader(content), hook: func() {
				results[i].readBody = true
				arrived()
				<-fence.release
			}}
			results[i].w = e.uploadWithToken(token, oid, int64(len(content)), body)
		})
	}
	wg.Wait()
	return results
}

// TestLFSUploadConcurrentDuplicate checks that two uploads of the same object
// that both get past the existence check both succeed. The second insert hits
// the unique constraint, which used to be answered with a 500.
func TestLFSUploadConcurrentDuplicate(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	const content = "concurrent object"
	oid := sha256Hex(content)

	for i, r := range e.uploadConcurrently(content, e.token, e.token) {
		if r.w.Code != http.StatusOK {
			t.Errorf("upload %d: got status %d, want 200: %s", i, r.w.Code, r.w.Body.String())
		}
		if !r.readBody {
			t.Errorf("upload %d returned without reading its body, so the uploads did not overlap", i)
		}
	}

	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid); err != nil {
		t.Errorf("object not recorded: %v", err)
	}
	e.assertNoTempFiles(t)
}

// TestLFSUploadConcurrentEarlyFailureReleasesFence checks the fence of
// uploadConcurrently: an upload that fails before reading its body must not
// leave the other one waiting. TestLFSUploadConcurrentDuplicate used to hang
// until the test timeout when one upload failed early.
func TestLFSUploadConcurrentEarlyFailureReleasesFence(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	const content = "fenced object"
	oid := sha256Hex(content)

	// The bad token fails authentication, before the body is read.
	results := e.uploadConcurrently(content, e.token, "bad-token")
	if r := results[0]; r.w.Code != http.StatusOK || !r.readBody {
		t.Errorf("good upload: got status %d, read body %v; want 200 after reading the body: %s",
			r.w.Code, r.readBody, r.w.Body.String())
	}
	if r := results[1]; r.w.Code != http.StatusForbidden || r.readBody {
		t.Errorf("bad-token upload: got status %d, read body %v; want 403 without reading the body: %s",
			r.w.Code, r.readBody, r.w.Body.String())
	}

	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid); err != nil {
		t.Errorf("object not recorded: %v", err)
	}
	e.assertNoTempFiles(t)
}

// TestLFSUploadRepoDeletedMidUpload checks that a foreign key violation is
// not mistaken for a duplicate upload, which the handler reports as success.
func TestLFSUploadRepoDeletedMidUpload(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	const content = "orphan object"
	oid := sha256Hex(content)

	body := &hookReader{r: strings.NewReader(content), hook: func() {
		if err := e.datastore.DeleteRepoByName(e.ctx, e.env.dbx, "repo"); err != nil {
			t.Errorf("delete repo: %v", err)
		}
	}}
	w := e.upload(oid, int64(len(content)), body)
	if w.Code == http.StatusOK {
		t.Fatalf("got 200 for an object whose repository was deleted")
	}
	e.assertNoTempFiles(t)
}

// batchUpload asks the batch endpoint to upload the given objects and returns
// the decoded response.
func (e *lfsUploadEnv) batchUpload(t *testing.T, objects ...lfs.Pointer) lfs.BatchResponse {
	t.Helper()
	body, err := json.Marshal(lfs.BatchRequest{Operation: lfs.OperationUpload, Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(e.ctx, http.MethodPost,
		"/repo.git/info/lfs/objects/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", lfs.MediaType)
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Authorization", "token "+e.token)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("batch: got status %d: %s", w.Code, w.Body.String())
	}
	var resp lfs.BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Objects) != len(objects) {
		t.Fatalf("batch: got %d objects, want %d", len(resp.Objects), len(objects))
	}
	return resp
}

// TestLFSBatchUploadOmitsActionsForPresentObjects checks that an upload batch
// leaves out actions for objects the server already stores, as the batch API
// asks, so the client does not send them again. Objects that are missing, or
// whose row or file is incomplete or truncated, still get upload and verify
// actions.
func TestLFSBatchUploadOmitsActionsForPresentObjects(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)

	stored := "stored object"
	storedOid := sha256Hex(stored)
	if w := e.upload(storedOid, int64(len(stored)), strings.NewReader(stored)); w.Code != http.StatusOK {
		t.Fatalf("upload: got status %d: %s", w.Code, w.Body.String())
	}

	// A row without its file, as left by a lost or deleted file.
	noFile := "row only"
	noFileOid := sha256Hex(noFile)
	if err := e.datastore.CreateLFSObject(e.ctx, e.env.dbx, e.repo.ID(), noFileOid, int64(len(noFile))); err != nil {
		t.Fatal(err)
	}

	// A row whose file was truncated: the upload handler restores such a
	// file, so the client must be asked to send it again.
	truncated := "truncated object"
	truncatedOid := sha256Hex(truncated)
	if w := e.upload(truncatedOid, int64(len(truncated)), strings.NewReader(truncated)); w.Code != http.StatusOK {
		t.Fatalf("upload: got status %d: %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(e.objectPath(truncatedOid), []byte(truncated[:4]), 0o600); err != nil {
		t.Fatal(err)
	}

	missingOid := sha256Hex("not uploaded")

	resp := e.batchUpload(t,
		lfs.Pointer{Oid: storedOid, Size: int64(len(stored))},
		lfs.Pointer{Oid: storedOid, Size: int64(len(stored)) + 1},
		lfs.Pointer{Oid: noFileOid, Size: int64(len(noFile))},
		lfs.Pointer{Oid: truncatedOid, Size: int64(len(truncated))},
		lfs.Pointer{Oid: missingOid, Size: 12},
	)

	wantActions := []bool{false, true, true, true, true}
	for i, o := range resp.Objects {
		if o.Error != nil {
			t.Errorf("object %d: unexpected error %+v", i, o.Error)
		}
		_, hasUpload := o.Actions[lfs.ActionUpload]
		_, hasVerify := o.Actions[lfs.ActionVerify]
		if hasUpload != wantActions[i] || hasVerify != wantActions[i] {
			t.Errorf("object %d (%s, size %d): upload=%v verify=%v, want %v",
				i, o.Oid, o.Size, hasUpload, hasVerify, wantActions[i])
		}
	}
}

// TestLFSUploadRestoresMissingObject checks that an upload of an object that
// is recorded but whose file is gone or damaged stores it again. The handler
// used to skip any upload of a recorded object, so the file could never be
// restored.
func TestLFSUploadRestoresMissingObject(t *testing.T) {
	t.Parallel()
	const content = "restore me"
	oid := sha256Hex(content)
	cases := []struct {
		name   string
		damage func(t *testing.T, objPath string)
	}{
		{"file removed", func(t *testing.T, objPath string) {
			t.Helper()
			if err := os.Remove(objPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"file truncated", func(t *testing.T, objPath string) {
			t.Helper()
			if err := os.WriteFile(objPath, []byte(content[:3]), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"file corrupted", func(t *testing.T, objPath string) {
			t.Helper()
			if err := os.WriteFile(objPath, []byte(strings.ToUpper(content)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newLFSUploadEnv(t)
			if w := e.upload(oid, int64(len(content)), strings.NewReader(content)); w.Code != http.StatusOK {
				t.Fatalf("first upload: got status %d, want 200: %s", w.Code, w.Body.String())
			}
			tc.damage(t, e.objectPath(oid))

			if w := e.upload(oid, int64(len(content)), strings.NewReader(content)); w.Code != http.StatusOK {
				t.Fatalf("second upload: got status %d, want 200: %s", w.Code, w.Body.String())
			}
			got, err := os.ReadFile(e.objectPath(oid))
			if err != nil {
				t.Fatalf("object not restored: %v", err)
			}
			if string(got) != content {
				t.Errorf("stored content %q, want %q", got, content)
			}
			if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid); err != nil {
				t.Errorf("object no longer recorded: %v", err)
			}
			e.assertNoTempFiles(t)
		})
	}
}

// TestLFSUploadAbortedBody checks an upload whose body cannot be read to the
// end, as when the client aborts it: that is the client's doing, so it gets
// a 400 and an info log line rather than a 500 and an error, and nothing is
// stored.
func TestLFSUploadAbortedBody(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := log.New(&logs)

	e := newLFSUploadEnvLogging(t, logger)
	const content = "hello lfs"
	oid := sha256Hex(content)

	body := io.MultiReader(strings.NewReader(content[:4]), iotest.ErrReader(io.ErrUnexpectedEOF))
	w := e.upload(oid, int64(len(content)), body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, lfs.MediaType) {
		t.Errorf("Content-Type = %q, want an LFS JSON error", ct)
	}
	if out := logs.String(); strings.Contains(out, "ERRO") || !strings.Contains(out, "error reading object") {
		t.Errorf("aborted upload not logged at info level:\n%s", out)
	}
	if _, err := os.Stat(e.objectPath(oid)); !os.IsNotExist(err) {
		t.Errorf("object stored from an aborted upload (stat err: %v)", err)
	}
	e.assertNoTempFiles(t)
}
