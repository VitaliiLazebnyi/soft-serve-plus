package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
	_ "modernc.org/sqlite"
)

// TestLFSLockLookupIsScopedToRepository verifies that a lock cannot be read by
// global lock ID from a repository it does not belong to.
//
// The LFS lock routes authorize the caller against the repository in the URL,
// and the lock list handler accepts an `id` query parameter, so it must not
// fetch the lock by global ID alone. A user with write access to any
// repository could otherwise walk lock IDs and read file paths, owner
// usernames, and timestamps out of private repositories they have no access
// to.
func TestLFSLockLookupIsScopedToRepository(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	// The victim owns a private repository with a lock on a sensitive path.
	victim, err := be.CreateUser(ctx, "victim", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	victimRepo, err := be.CreateRepository(ctx, "victim-repo", victim, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := datastore.CreateLFSLockForUser(ctx, dbx, victimRepo.ID(), victim.ID(), "secret/plans.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}

	victimLock, err := datastore.GetLFSLockForPath(ctx, dbx, victimRepo.ID(), "secret/plans.bin")
	if err != nil {
		t.Fatal(err)
	}

	// The attacker owns their own repository, so they have write access to
	// it, but no access at all to the victim's repository.
	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attackerRepo, err := be.CreateRepository(ctx, "attacker-repo", attacker, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The store must not return a lock that belongs to another repository.
	_, err = datastore.GetLFSLockByID(ctx, dbx, attackerRepo.ID(), victimLock.ID)
	if err == nil {
		t.Fatal("expected error reading another repository's lock by ID")
	}

	// The owner can still read their own lock by ID.
	got, err := datastore.GetLFSLockByID(ctx, dbx, victimRepo.ID(), victimLock.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := got.Path; got != "secret/plans.bin" {
		t.Fatalf("got.Path = %v, want %v", got, "secret/plans.bin")
	}
}

// TestLFSLocksGetDoesNotLeakAcrossRepositories drives the HTTP lock list
// handler the way an attacker would: authorized for their own repository,
// asking for a lock ID that belongs to somebody else's private repository.
func TestLFSLocksGetDoesNotLeakAcrossRepositories(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	victim, err := be.CreateUser(ctx, "victim", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	victimRepo, err := be.CreateRepository(ctx, "victim-repo", victim, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := datastore.CreateLFSLockForUser(ctx, dbx, victimRepo.ID(), victim.ID(), "secret/plans.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	victimLock, err := datastore.GetLFSLockForPath(ctx, dbx, victimRepo.ID(), "secret/plans.bin")
	if err != nil {
		t.Fatal(err)
	}

	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attackerRepo, err := be.CreateRepository(ctx, "attacker-repo", attacker, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Request the victim's lock ID while scoped to the attacker's own
	// repository, which is exactly what the route authorizes.
	s := env.server(t)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/attacker-repo.git/info/lfs/locks?id="+strconv.FormatInt(victimLock.ID, 10), nil)
	req.Header.Set("Accept", lfs.MediaType)

	w := httptest.NewRecorder()
	s.serveRoute((*server).serviceLfsLocks, w, req, routeCaller{user: attacker, repo: attackerRepo})

	body := w.Body.String()
	if strings.Contains(body, "secret/plans.bin") {
		t.Errorf("leaked victim lock path in response: %s", body)
	}
	if strings.Contains(body, "victim") {
		t.Errorf("leaked victim username in response: %s", body)
	}

	// The owner asking for their own lock still gets it, so the fix did not
	// simply break the endpoint.
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/victim-repo.git/info/lfs/locks?id="+strconv.FormatInt(victimLock.ID, 10), nil)
	req.Header.Set("Accept", lfs.MediaType)

	w = httptest.NewRecorder()
	s.serveRoute((*server).serviceLfsLocks, w, req, routeCaller{user: victim, repo: victimRepo})
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}

	var resp lfs.LockListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Locks); got != 1 {
		t.Fatalf("len(resp.Locks) = %v, want %v", got, 1)
	}
	if got := resp.Locks[0].Path; got != "secret/plans.bin" {
		t.Fatalf("resp.Locks[0].Path = %v, want %v", got, "secret/plans.bin")
	}
}

// TestLFSLocksDeleteDoesNotLeakAcrossRepositories covers the unlock route,
// which fetched the lock by global ID and echoed its path and owner back in
// the "lock belongs to another user" rejection. Deletion itself was already
// repository-scoped, so this path disclosed metadata rather than destroying
// it, but it is the same unscoped lookup.
func TestLFSLocksDeleteDoesNotLeakAcrossRepositories(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	victim, err := be.CreateUser(ctx, "victim", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	victimRepo, err := be.CreateRepository(ctx, "victim-repo", victim, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := datastore.CreateLFSLockForUser(ctx, dbx, victimRepo.ID(), victim.ID(), "secret/plans.bin", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	victimLock, err := datastore.GetLFSLockForPath(ctx, dbx, victimRepo.ID(), "secret/plans.bin")
	if err != nil {
		t.Fatal(err)
	}

	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attackerRepo, err := be.CreateRepository(ctx, "attacker-repo", attacker, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/attacker-repo.git/info/lfs/locks/"+strconv.FormatInt(victimLock.ID, 10)+"/unlock",
		strings.NewReader(`{"force":false}`))
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Content-Type", lfs.MediaType)
	req = mux.SetURLVars(req, map[string]string{"lock_id": strconv.FormatInt(victimLock.ID, 10)})

	w := httptest.NewRecorder()
	env.server(t).serveRoute((*server).serviceLfsLocksDelete, w, req, routeCaller{user: attacker, repo: attackerRepo})

	// Guard against the request never reaching the lookup: a missing or
	// unparseable lock_id would make this test vacuously pass.
	if got := w.Code; got != http.StatusNotFound {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusNotFound)
	}

	body := w.Body.String()
	if strings.Contains(body, "secret/plans.bin") {
		t.Errorf("leaked victim lock path in unlock response: %s", body)
	}
	if strings.Contains(body, "victim") {
		t.Errorf("leaked victim username in unlock response: %s", body)
	}

	// The victim's lock must still exist.
	still, err := datastore.GetLFSLockByID(ctx, dbx, victimRepo.ID(), victimLock.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := still.Path; got != "secret/plans.bin" {
		t.Fatalf("still.Path = %v, want %v", got, "secret/plans.bin")
	}
}

// forgeableServices are the service names that select a non-LFS branch in
// withAccess. None of them may change how an LFS route is authorized.
var forgeableServices = []string{"git-upload-pack", "git-receive-pack", "git-upload-archive"}

// TestLFSUploadRejectsForgedServiceParam covers an authorization bypass in
// withAccess: the LFS branch was selected by a `service` value that, on an LFS
// route, always came from a caller-supplied query parameter. Because the
// git-upload-pack branch sat earlier in the same switch, appending
// `?service=git-upload-pack` matched that branch instead and skipped every LFS
// write check. Under the shipped defaults (anon-access read-only, LFS enabled)
// an unauthenticated caller could write objects into any public repository.
func TestLFSUploadRejectsForgedServiceParam(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	cfg := env.cfg
	dbx := env.dbx

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "victim-repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	oid := strings.Repeat("a", 64)
	path := "/victim-repo.git/info/lfs/objects/basic/" + oid

	upload := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader("payload"))
		req.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Baseline: anonymous read-only access cannot upload. It is asked for
	// credentials (401), which git-lfs needs before it will send any.
	if got := upload(path).Code; got != http.StatusUnauthorized {
		t.Fatalf("upload(path).Code = %v, want %v", got, http.StatusUnauthorized)
	}

	for _, service := range forgeableServices {
		w := upload(path + "?service=" + service)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("service=%s: got status %d, want 401: %s", service, w.Code, w.Body.String())
		}
	}

	// The object must not exist on disk or in the database. An error status
	// alone does not prove the handler stopped before storing anything.
	objPath := filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10),
		"objects", oid[0:2], oid[2:4], oid)
	if _, err := os.Stat(objPath); !os.IsNotExist(err) {
		t.Errorf("object written to disk at %s despite rejection", objPath)
	}
	if _, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), oid); err == nil {
		t.Error("object registered in database despite rejection")
	}
}

// TestLFSLockCreateRejectsForgedServiceParam is the lock-create half of the
// same bypass: a read-only collaborator could take locks on arbitrary paths.
func TestLFSLockCreateRejectsForgedServiceParam(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "victim-repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// An authenticated user with no more than read access to the repository.
	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, attacker, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)

	lock := func(url string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, url,
			strings.NewReader(`{"path":"src/critical.bin","ref":{"name":"refs/heads/main"}}`))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		req.Header.Set("Authorization", "token "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if got := lock("/victim-repo.git/info/lfs/locks").Code; got != http.StatusForbidden {
		t.Fatalf("lock(\"/victim-repo.git/info/lfs/locks\").Code = %v, want %v", got, http.StatusForbidden)
	}

	for _, service := range forgeableServices {
		w := lock("/victim-repo.git/info/lfs/locks?service=" + service)
		if w.Code != http.StatusForbidden {
			t.Errorf("service=%s: got status %d, want 403: %s", service, w.Code, w.Body.String())
		}
	}

	if _, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), "src/critical.bin"); err == nil {
		t.Error("lock created despite rejection")
	}
}

// TestLFSDisabledRejectsForgedServiceParam checks the administrative kill
// switch. The `lfs.enabled = false` check lived inside the branch the bypass
// skipped, so a forged service value re-enabled LFS on a server where the
// administrator had turned it off.
func TestLFSDisabledRejectsForgedServiceParam(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be

	cfg := env.cfg
	cfg.LFS.Enabled = false

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "victim-repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	url := "/victim-repo.git/info/lfs/objects/basic/" + strings.Repeat("a", 64)

	for _, service := range forgeableServices {
		req := httptest.NewRequestWithContext(ctx, http.MethodPut, url+"?service="+service,
			strings.NewReader("payload"))
		req.Header.Set("Content-Type", "application/octet-stream")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("service=%s: got status %d, want 404 with LFS disabled: %s", service, w.Code, w.Body.String())
		}
	}
}

// TestLFSWriteStillWorksForAuthorizedUser guards against the fix simply
// breaking LFS: a user with write access must still be able to upload an
// object and take a lock, with or without a service parameter present.
func TestLFSWriteStillWorksForAuthorizedUser(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "owner-repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, owner, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	body := "payload"
	oid := "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" // sha256(body)

	req := httptest.NewRequestWithContext(ctx, http.MethodPut,
		"/owner-repo.git/info/lfs/objects/basic/"+oid, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Set("Authorization", "token "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}

	obj, err := datastore.GetLFSObjectByOid(ctx, dbx, repo.ID(), oid)
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.Oid; got != oid {
		t.Fatalf("obj.Oid = %v, want %v", got, oid)
	}

	// A service parameter on an LFS route is ignored, not rejected.
	req = httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/owner-repo.git/info/lfs/locks?service=git-upload-pack",
		strings.NewReader(`{"path":"src/file.bin","ref":{"name":"refs/heads/main"}}`))
	req.Header.Set("Content-Type", lfs.MediaType)
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Authorization", "token "+token)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Code; got != http.StatusCreated {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusCreated)
	}

	got, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), "src/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := got.Path; got != "src/file.bin" {
		t.Fatalf("got.Path = %v, want %v", got, "src/file.bin")
	}
}

// TestLFSWriteDeniedAsksAnonymousForCredentials checks how an LFS write is
// refused. git-lfs sends its first request without credentials and only asks
// for them after a 401, so an anonymous caller must get a 401 with a
// challenge; a 403 would end a push from a user whose token is in a
// credential helper. A signed-in caller without write access still gets 403.
//
// Its subtests run in order on one fixture, so only the test runs in
// parallel with others.
func TestLFSWriteDeniedAsksAnonymousForCredentials(t *testing.T) { //nolint:tparallel // the subtests share one fixture and run in order
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "public-repo", owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	reader, err := be.CreateUser(ctx, "reader", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, reader, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	oid := strings.Repeat("b", 64)
	requests := []struct {
		name, method, path, body string
	}{
		{
			"batch upload", http.MethodPost, "/public-repo.git/info/lfs/objects/batch",
			`{"operation":"upload","objects":[{"oid":"` + oid + `","size":1}]}`,
		},
		{"basic upload", http.MethodPut, "/public-repo.git/info/lfs/objects/basic/" + oid, "x"},
		{
			"lock create", http.MethodPost, "/public-repo.git/info/lfs/locks",
			`{"path":"a.bin","ref":{"name":"refs/heads/main"}}`,
		},
	}
	for _, rq := range requests {
		for _, caller := range []struct {
			name       string
			auth       string
			wantStatus int
		}{
			{"anonymous", "", http.StatusUnauthorized},
			{"reader", "token " + token, http.StatusForbidden},
		} {
			t.Run(rq.name+"/"+caller.name, func(t *testing.T) {
				req := httptest.NewRequestWithContext(ctx, rq.method, rq.path, strings.NewReader(rq.body))
				req.Header.Set("Accept", lfs.MediaType)
				req.Header.Set("Content-Type", lfs.MediaType)
				if caller.auth != "" {
					req.Header.Set("Authorization", caller.auth)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != caller.wantStatus {
					t.Fatalf("got status %d, want %d: %s", w.Code, caller.wantStatus, w.Body.String())
				}
				challenged := w.Header().Get("WWW-Authenticate") != ""
				if anonymous := caller.auth == ""; challenged != anonymous {
					t.Errorf("WWW-Authenticate set=%v, want %v", challenged, anonymous)
				}
			})
		}
	}
}

// TestLFSBatchRejectsOversizedRequests checks the batch endpoint's limits. Each
// object costs a storage stat and a database lookup, and anyone who can read a
// repository may call the endpoint, so the number of objects and the body size
// are capped.
func TestLFSBatchRejectsOversizedRequests(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	batch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost,
			"/repo.git/info/lfs/objects/batch", strings.NewReader(body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	objects := func(n int) string {
		objs := make([]string, n)
		for i := range objs {
			objs[i] = fmt.Sprintf(`{"oid":"%064x","size":1}`, i)
		}
		return `{"operation":"download","objects":[` + strings.Join(objs, ",") + `]}`
	}

	w := batch(objects(maxLfsBatchObjects))
	if got := w.Code; got != http.StatusOK {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
	}

	w = batch(objects(maxLfsBatchObjects + 1))
	if got := w.Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusRequestEntityTooLarge)
	}

	big := `{"operation":"download","ref":{"name":"` + strings.Repeat("a", maxLfsBatchBodySize) + `"},"objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}]}`
	w = batch(big)
	if got := w.Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusRequestEntityTooLarge)
	}

	// The object limit applies to uploads too, before the operation is
	// handled.
	w = batch(strings.Replace(objects(maxLfsBatchObjects+1), "download", "upload", 1))
	if got := w.Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("w.Code = %v, want %v", got, http.StatusRequestEntityTooLarge)
	}
}

// TestLFSJSONBodiesAreLimited checks that the verify and lock endpoints, which
// take a single object or path, refuse oversized JSON bodies instead of
// reading them into memory.
func TestLFSJSONBodiesAreLimited(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, owner, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	padding := strings.Repeat("a", maxLfsJSONBodySize)
	for _, tc := range []struct {
		name, path, body string
		auth             bool
	}{
		// Anonymous readers can reach verify under the default anon access.
		{"verify", "/repo.git/info/lfs/objects/basic/verify", `{"oid":"` + padding + `","size":1}`, false},
		{"lock create", "/repo.git/info/lfs/locks", `{"path":"` + padding + `"}`, true},
		{"lock verify", "/repo.git/info/lfs/locks/verify", `{"cursor":"` + padding + `"}`, true},
		{"unlock", "/repo.git/info/lfs/locks/1/unlock", `{"force":false,"ref":{"name":"` + padding + `"}}`, true},
	} {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		if tc.auth {
			req.Header.Set("Authorization", "token "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: got status %d, want 413: %s", tc.name, w.Code, w.Body.String())
		}
	}
}

// TestLFSVerifyRejectsStoredSizeMismatch checks that verify fails when the
// object recorded in the database matches the requested size but the file on
// disk does not. A handler that fell through without writing a response
// would get an implicit 200 from net/http, so a truncated or corrupt object
// would pass verification.
func TestLFSVerifyRejectsStoredSizeMismatch(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	pointer := lfs.Pointer{
		Oid:  "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393",
		Size: 12,
	}
	dbx := env.dbx
	if err := datastore.CreateLFSObject(ctx, dbx, repo.ID(), pointer.Oid, pointer.Size); err != nil {
		t.Fatal(err)
	}

	objPath := filepath.Join(env.cfg.DataPath, "lfs",
		strconv.FormatInt(repo.ID(), 10), "objects", filepath.FromSlash(pointer.RelativePath()))
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	verify := func() *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"oid":%q,"size":%d}`, pointer.Oid, pointer.Size)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/objects/basic/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Truncated on disk: must not verify.
	if err := os.WriteFile(objPath, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := verify(); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("truncated object: got status %d, want 422: %s", w.Code, w.Body.String())
	}

	// Intact on disk: verifies.
	if err := os.WriteFile(objPath, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := verify(); w.Code != http.StatusOK {
		t.Fatalf("intact object: got status %d, want 200: %s", w.Code, w.Body.String())
	}
}

// A download batch offers an object only if its file has the requested size:
// a short file, such as one a failed write left behind, is not offered
// because it exists and its row has the right size.
func TestLFSBatchDownloadShortObjectIsNotOffered(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore
	dbx := env.dbx

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	content := "payload"
	oid := "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" // sha256(content)
	if err := datastore.CreateLFSObject(ctx, dbx, repo.ID(), oid, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	cfg := env.cfg
	objPath := filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10), "objects", oid[0:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatal(err)
	}

	router := env.gitRouter(t)
	batch := func() lfs.ObjectResponse {
		t.Helper()
		body := fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":%d}]}`, oid, len(content))
		req := httptest.NewRequestWithContext(ctx, http.MethodPost,
			"/repo.git/info/lfs/objects/batch", strings.NewReader(body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if got := w.Code; got != http.StatusOK {
			t.Fatalf("w.Code = %v, want %v", got, http.StatusOK)
		}
		var resp lfs.BatchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Objects); got != 1 {
			t.Fatalf("len(resp.Objects) = %v, want %v", got, 1)
		}
		return *resp.Objects[0]
	}

	if err := os.WriteFile(objPath, []byte(content[:3]), 0o600); err != nil {
		t.Fatal(err)
	}
	got := batch()
	if got.Error == nil || got.Actions[lfs.ActionDownload] != nil {
		t.Fatalf("short object: got error %+v, actions %v; want an error and no download", got.Error, got.Actions)
	}

	if err := os.WriteFile(objPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got = batch()
	if got.Error != nil || got.Actions[lfs.ActionDownload] == nil {
		t.Fatalf("complete object: got error %+v, actions %v; want a download", got.Error, got.Actions)
	}
}

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
// verifies the body against the oid and the declared size, so a client
// cannot store arbitrary content under any oid.
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
// the unique constraint, which is not answered with a 500.
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
// is recorded but whose file is gone or damaged stores it again, rather than
// skip it because the object is recorded.
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

// TestLFSBatchActionsForwardOnlyJWT checks which credential a batch response
// copies into its actions. An access token sent with the
// batch request must not come back in the response body, where client traces
// and proxies can log it. A bearer JWT, which is how a client authenticated
// over SSH reaches the object URLs, is forwarded with its remaining lifetime
// in expires_in.
func TestLFSBatchActionsForwardOnlyJWT(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be, datastore := env.ctx, env.be, env.datastore

	cfg := env.cfg
	env.writeHostKey(t)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, owner, "t", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	jwtToken := env.mintJWT(t, owner, "repo")

	// An object that exists, so a download batch returns an action.
	body := "payload"
	oid := "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" // sha256(body)
	repo, err := be.Repository(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	objPath := filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10), "objects",
		filepath.FromSlash(lfs.Pointer{Oid: oid}.RelativePath()))
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := datastore.CreateLFSObject(ctx, env.dbx, repo.ID(), oid, int64(len(body))); err != nil {
		t.Fatal(err)
	}

	// An object the server does not have, so an upload batch returns
	// actions: objects already stored get none.
	missingOid := sha256Hex("missing")

	router := env.gitRouter(t)
	batch := func(op, auth string) lfs.BatchResponse {
		t.Helper()
		reqOid := oid
		if op == lfs.OperationUpload {
			reqOid = missingOid
		}
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/objects/batch",
			strings.NewReader(`{"operation":"`+op+`","objects":[{"oid":"`+reqOid+`","size":7}]}`))
		req.Header.Set("Accept", lfs.MediaType)
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s batch: got status %d: %s", op, w.Code, w.Body.String())
		}
		var resp lfs.BatchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Objects); got != 1 {
			t.Fatalf("len(resp.Objects) = %v, want %v", got, 1)
		}
		if resp.Objects[0].Error != nil {
			t.Fatalf("%s batch: object error: %+v", op, resp.Objects[0].Error)
		}
		return resp
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("owner:"+token))
	for _, auth := range []string{basic, "token " + token} {
		for _, op := range []string{lfs.OperationDownload, lfs.OperationUpload} {
			resp := batch(op, auth)
			if len(resp.Objects[0].Actions) <= 0 {
				t.Fatal("not true: len(resp.Objects[0].Actions) > 0")
			}
			for name, action := range resp.Objects[0].Actions {
				if _, ok := action.Header["Authorization"]; ok {
					t.Errorf("%s %s action echoes the caller's %q credential", op, name, strings.Fields(auth)[0])
				}
				if action.ExpiresIn != nil {
					t.Fatal("not true: action.ExpiresIn == nil")
				}
			}
		}
	}
	// The upload action keeps its Content-Type without a credential.
	up := batch(lfs.OperationUpload, basic).Objects[0].Actions[lfs.ActionUpload]
	if got := up.Header["Content-Type"]; got != "application/octet-stream" {
		t.Fatalf("up.Header[\"Content-Type\"] = %v, want %v", got, "application/octet-stream")
	}

	for _, op := range []string{lfs.OperationDownload, lfs.OperationUpload} {
		resp := batch(op, "Bearer "+jwtToken)
		if len(resp.Objects[0].Actions) <= 0 {
			t.Fatal("not true: len(resp.Objects[0].Actions) > 0")
		}
		for name, action := range resp.Objects[0].Actions {
			if got := action.Header["Authorization"]; got != "Bearer "+jwtToken {
				t.Errorf("%s %s action: Authorization = %q, want the JWT", op, name, got)
			}
			if action.ExpiresIn == nil {
				t.Fatalf("%s %s action: no expires_in", op, name)
			}
			// mintJWT tokens last an hour.
			if in := *action.ExpiresIn; in <= 0 || in > int64(time.Hour/time.Second) {
				t.Errorf("%s %s action: expires_in = %d, want within an hour", op, name, in)
			}
		}
	}
}

// TestLFSActionAuthFor checks the credential choice and the expires_in value
// directly, including tokens close to or past their expiry.
func TestLFSActionAuthFor(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, be := env.ctx, env.be
	env.writeHostKey(t)
	s := env.server(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	jwtToken := env.mintJWT(t, owner, "repo") // expires in an hour
	otherRepo := env.mintJWT(t, owner, "other")
	// The same claims with the signature of another token: parsing it
	// without checking the signature would accept it.
	forged := jwtToken[:strings.LastIndex(jwtToken, ".")] + otherRepo[strings.LastIndex(otherRepo, "."):]

	now := time.Now()
	for _, tc := range []struct {
		name       string
		header     string
		now        time.Time
		forward    bool
		expiresMin int64
		expiresMax int64
	}{
		{name: "none"},
		{name: "basic", header: "Basic b3duZXI6aHVudGVyMg=="},
		{name: "token", header: "token ss_abc"},
		{name: "bearer garbage", header: "Bearer garbage"},
		{name: "bearer empty", header: "Bearer "},
		{name: "bearer for another repository", header: "Bearer " + otherRepo},
		{name: "bearer with a forged signature", header: "Bearer " + forged},
		{name: "bearer", header: "Bearer " + jwtToken, now: now, forward: true, expiresMin: 3590, expiresMax: 3600},
		{name: "lowercase bearer", header: "bearer " + jwtToken, now: now, forward: true, expiresMin: 3590, expiresMax: 3600},
		{name: "nearly expired", header: "Bearer " + jwtToken, now: now.Add(59 * time.Minute), forward: true, expiresMin: 1, expiresMax: 60},
		{name: "expired", header: "Bearer " + jwtToken, now: now.Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/objects/batch", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if tc.now.IsZero() {
				tc.now = now
			}
			auth := s.lfsActionAuthFor(req, s.logger, "repo", tc.now)
			link := auth.link("http://localhost/x", nil)
			if !tc.forward {
				if link.Header != nil || link.ExpiresIn != nil {
					t.Fatalf("forwarded %q: header %v expires_in %v", tc.header, link.Header, link.ExpiresIn)
				}
				return
			}
			if got := link.Header["Authorization"]; got != "Bearer "+jwtToken {
				t.Fatalf("Authorization = %q", got)
			}
			if link.ExpiresIn == nil || *link.ExpiresIn < tc.expiresMin || *link.ExpiresIn > tc.expiresMax {
				t.Fatalf("expires_in = %v, want %d..%d", link.ExpiresIn, tc.expiresMin, tc.expiresMax)
			}
		})
	}
}

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
// recording it) records nothing. Recording the size the client sent would
// let an anonymous reader pin a wrong size: later downloads would get "size
// mismatch" and uploads of the object would be skipped.
func TestLFSDownloadBatchDoesNotRecordObjects(t *testing.T) {
	t.Parallel()
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
		_, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid)
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
	rec, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Size != size {
		t.Errorf("recorded size %d, want %d", rec.Size, size)
	}
}

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
// serves a stored object whose database row is missing, with the length of
// the file.
func TestLFSDownloadWithoutDatabaseRow(t *testing.T) {
	t.Parallel()
	const content = "hello lfs download"
	e := newLFSUploadEnv(t)
	oid := e.storeObject(t, content)
	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, e.env.dbx, e.repo.ID(), oid); err == nil {
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

// countingStore counts the per-item lookups an LFS request makes.
type countingStore struct {
	store.Store
	objectLookups atomic.Int32
	userLookups   atomic.Int32
}

func (s *countingStore) GetLFSObjectByOid(ctx context.Context, h db.Handler, repoID int64, oid string) (models.LFSObject, error) {
	s.objectLookups.Add(1)
	return s.Store.GetLFSObjectByOid(ctx, h, repoID, oid)
}

func (s *countingStore) GetUserByID(ctx context.Context, h db.Handler, id int64) (models.User, error) {
	s.userLookups.Add(1)
	return s.Store.GetUserByID(ctx, h, id)
}

// withCountingStore routes e's requests through a countingStore.
func (e *lfsUploadEnv) withCountingStore(t *testing.T) *countingStore {
	t.Helper()
	cs := &countingStore{Store: e.datastore}
	s := e.env.server(t)
	s.datastore = cs
	e.router = s.gitRouter()
	return cs
}

// A batch looks its objects up in one query, not one per object: a batch
// can hold 1,000 objects.
func TestLFSBatchLooksObjectsUpTogether(t *testing.T) {
	t.Parallel()
	e := newLFSUploadEnv(t)
	var objects []lfs.Pointer
	for _, s := range []string{"one", "two", "three"} {
		oid := sha256Hex(s)
		if w := e.upload(oid, int64(len(s)), strings.NewReader(s)); w.Code != http.StatusOK {
			t.Fatalf("upload: got status %d: %s", w.Code, w.Body.String())
		}
		objects = append(objects, lfs.Pointer{Oid: oid, Size: int64(len(s))})
	}
	objects = append(objects, lfs.Pointer{Oid: sha256Hex("missing"), Size: 7})
	cs := e.withCountingStore(t)

	for _, op := range []string{lfs.OperationUpload, lfs.OperationDownload} {
		resp := e.batch(t, op, objects...)
		for i, o := range resp.Objects {
			stored := i < 3
			switch op {
			case lfs.OperationUpload:
				if stored != (o.Actions == nil) {
					t.Errorf("upload batch: object %d actions = %v, want present = %v", i, o.Actions, stored)
				}
			case lfs.OperationDownload:
				if stored != (o.Error == nil) {
					t.Errorf("download batch: object %d error = %v, want found = %v", i, o.Error, stored)
				}
			}
		}
	}
	if n := cs.objectLookups.Load(); n != 0 {
		t.Errorf("per-object lookups = %d, want 0", n)
	}
}

// batch posts a batch request for op and returns the decoded response.
func (e *lfsUploadEnv) batch(t *testing.T, op string, objects ...lfs.Pointer) lfs.BatchResponse {
	t.Helper()
	body, err := json.Marshal(lfs.BatchRequest{Operation: op, Objects: objects})
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
	return resp
}

// Lock responses name each lock's owner from the lock query itself, not one
// user lookup per lock.
func TestLFSLocksNameOwnersWithoutLookups(t *testing.T) {
	t.Parallel()
	f := newLocksFixture(t)
	ctx, rc := f.ctx, f.owner
	repo := rc.repo
	cs := &countingStore{Store: f.env.datastore}
	f.s.datastore = cs

	for _, route := range []struct {
		method, target string
		h              gitHandler
	}{
		{http.MethodGet, "/repo.git/info/lfs/locks", (*server).serviceLfsLocksGet},
		{http.MethodGet, "/repo.git/info/lfs/locks?path=a.bin", (*server).serviceLfsLocksGet},
		{http.MethodPost, "/repo.git/info/lfs/locks/verify", (*server).serviceLfsLocksVerify},
	} {
		var body io.Reader
		if route.method == http.MethodPost {
			body = strings.NewReader(`{}`)
		}
		req := httptest.NewRequestWithContext(ctx, route.method, route.target, body)
		req.Header.Set("Accept", lfs.MediaType)
		req.Header.Set("Content-Type", lfs.MediaType)
		w := httptest.NewRecorder()
		f.s.serveRoute(route.h, w, req, rc)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", route.method, route.target, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), `"name":"owner"`) {
			t.Errorf("%s %s: body %s does not name the owner", route.method, route.target, w.Body)
		}
	}
	_ = repo
	if n := cs.userLookups.Load(); n != 0 {
		t.Errorf("owner lookups = %d, want 0", n)
	}
}
