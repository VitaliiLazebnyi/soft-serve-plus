package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/gorilla/mux"
	"github.com/matryer/is"
)

// newLocksParamsContext returns a request context for a user who owns a
// repository holding two locks.
func newLocksParamsContext(t *testing.T) context.Context {
	t.Helper()
	is := is.New(t)
	ctx, be, datastore := newLFSTestContext(t)
	dbx := db.FromContext(ctx)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "a.bin", "refs/heads/main"))
	is.NoErr(datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "b.bin", "refs/heads/main"))

	ctx = proto.WithUserContext(ctx, owner)
	return proto.WithRepositoryContext(ctx, repo)
}

// TestLFSLocksGetRejectsInvalidParams checks that the lock list rejects an
// id, cursor or limit it cannot use. These used to be parsed with their
// errors ignored, so ?id=abc read as no id and listed every lock instead
// of looking one up.
func TestLFSLocksGetRejectsInvalidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	for _, query := range []string{
		"id=abc",
		"id=0",
		"id=-1",
		"id=99999999999999999999",
		"cursor=abc",
		"cursor=0",
		"cursor=-1",
		"cursor=9223372036854775807",
		"limit=abc",
		"limit=-1",
		"limit=99999999999999999999",
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?"+query, nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body)
			}
			var resp lfs.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Message == "" {
				t.Errorf("body is not an LFS error (%v): %+v", err, resp)
			}
		})
	}
}

// TestLFSLocksGetValidParams checks that valid paging still works, and that
// a limit above the maximum is lowered rather than rejected.
func TestLFSLocksGetValidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	for query, want := range map[string]struct {
		locks  int
		cursor string
	}{
		"":                  {locks: 2},
		"limit=1":           {locks: 1, cursor: "2"},
		"cursor=2&limit=1":  {locks: 1, cursor: "3"},
		"cursor=3&limit=1":  {locks: 0},
		"limit=1000":        {locks: 2},
		"cursor=1&limit=0":  {locks: 2},
		"cursor=21474836":   {locks: 0},
		"path=a.bin":        {locks: 1},
		"path=a.bin&id=":    {locks: 1},
		"cursor=&limit=":    {locks: 2},
		"limit=2&cursor=01": {locks: 2, cursor: "2"},
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?"+query, nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var resp lfs.LockListResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Locks) != want.locks || resp.NextCursor != want.cursor {
				t.Errorf("got %d locks, next_cursor %q; want %d, %q", len(resp.Locks), resp.NextCursor, want.locks, want.cursor)
			}
		})
	}
}

// TestLFSLocksVerifyRejectsInvalidParams covers the same cursor and limit
// checks on the verify endpoint, where an invalid cursor used to be read as
// the first page.
func TestLFSLocksVerifyRejectsInvalidParams(t *testing.T) {
	ctx := newLocksParamsContext(t)

	verify := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Accept", lfs.MediaType)
		w := httptest.NewRecorder()
		serviceLfsLocksVerify(w, req)
		return w
	}

	for _, body := range []string{
		`{"cursor":"abc"}`,
		`{"cursor":"0"}`,
		`{"cursor":"-1"}`,
		`{"cursor":"9223372036854775807"}`,
		`{"limit":-1}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := verify(body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body)
			}
			var resp lfs.ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil || resp.Message == "" {
				t.Errorf("body is not an LFS error (%v): %+v", err, resp)
			}
		})
	}

	w := verify(`{"cursor":"2","limit":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
	}
	var resp lfs.LockVerifyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Ours) != 1 || len(resp.Theirs) != 0 || resp.NextCursor != "3" {
		t.Errorf("got ours %d, theirs %d, next_cursor %q; want 1, 0, \"3\"", len(resp.Ours), len(resp.Theirs), resp.NextCursor)
	}
}

// fullPageLockStore returns a full page of the repository's first lock for
// any page, standing in for a repository with more locks than the last page
// can reach.
type fullPageLockStore struct{ store.Store }

func (s fullPageLockStore) GetLFSLocks(ctx context.Context, h db.Handler, repoID int64, _ int, limit int) ([]models.LFSLock, error) {
	first, err := s.Store.GetLFSLocks(ctx, h, repoID, 1, 1)
	if err != nil || len(first) == 0 {
		return first, err
	}
	locks := make([]models.LFSLock, limit)
	for i := range locks {
		locks[i] = first[0]
	}
	return locks, nil
}

// TestLFSLocksLastPage checks that a full page at the largest cursor the
// server accepts hands out no next_cursor, on both the list and the verify
// endpoint. It used to hand out lfs.MaxLocksCursor+1, which the next request
// was then refused for.
func TestLFSLocksLastPage(t *testing.T) {
	ctx := newLocksParamsContext(t)
	ctx = store.WithContext(ctx, fullPageLockStore{store.FromContext(ctx)})

	for cursor, want := range map[int]string{
		lfs.MaxLocksCursor - 1: strconv.Itoa(lfs.MaxLocksCursor),
		lfs.MaxLocksCursor:     "",
	} {
		t.Run(strconv.Itoa(cursor), func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?limit=1&cursor="+strconv.Itoa(cursor), nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("list status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var list lfs.LockListResponse
			if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(list.Locks) != 1 || list.NextCursor != want {
				t.Errorf("list got %d locks, next_cursor %q; want 1, %q", len(list.Locks), list.NextCursor, want)
			}

			body := fmt.Sprintf(`{"cursor":"%d","limit":1}`, cursor)
			req = httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/verify", strings.NewReader(body))
			req.Header.Set("Content-Type", lfs.MediaType)
			req.Header.Set("Accept", lfs.MediaType)
			w = httptest.NewRecorder()
			serviceLfsLocksVerify(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("verify status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var verify lfs.LockVerifyResponse
			if err := json.NewDecoder(w.Body).Decode(&verify); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(verify.Ours) != 1 || verify.NextCursor != want {
				t.Errorf("verify got %d ours, next_cursor %q; want 1, %q", len(verify.Ours), verify.NextCursor, want)
			}
		})
	}
}

// TestLFSLocksListNeedsOnlyReadAccess checks that a reader may list locks,
// as the locking API asks for, while creating one still needs write access.
// The list used to be treated as a write, so `git lfs locks` failed for
// readers over HTTP although it worked over SSH.
func TestLFSLocksListNeedsOnlyReadAccess(t *testing.T) {
	ctx, be, _ := newLFSTestContext(t)

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

	router := lfsTestRouter(ctx)
	for _, auth := range []string{"", "token " + token} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/public-repo.git/info/lfs/locks", nil)
		req.Header.Set("Accept", lfs.MediaType)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("auth %q: status = %d, want %d: %s", auth, w.Code, http.StatusOK, w.Body)
		}
	}

	if got := authorize("", "info/lfs/locks", http.MethodGet, true, access.ReadOnlyAccess, false, false); got != serve {
		t.Errorf("reader lock list: got %v, want serve", got)
	}
	if got := authorize("", "info/lfs/locks", http.MethodPost, true, access.ReadOnlyAccess, false, false); got != forbidLFSWrite {
		t.Errorf("reader lock create: got %v, want forbidLFSWrite", got)
	}
}

// TestLFSLocksListFilterNoMatch checks that a path or id filter matching no
// lock answers an empty list, not 404.
func TestLFSLocksListFilterNoMatch(t *testing.T) {
	ctx := newLocksParamsContext(t)

	for _, query := range []string{"path=missing.bin", "id=12345"} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?"+query, nil)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocks(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var resp map[string]json.RawMessage
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := string(resp["locks"]); got != "[]" {
				t.Errorf("locks = %s, want []", got)
			}
		})
	}
}

// TestLFSLockCreateConflictReturnsLock checks that creating a lock on a path
// someone else holds answers 409 with that lock, as the spec asks. The
// lookup used to be scoped to the caller, so the response carried an empty
// lock.
func TestLFSLockCreateConflictReturnsLock(t *testing.T) {
	ctx := newLocksParamsContext(t)
	other, err := backend.FromContext(ctx).CreateUser(ctx, "other", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx = access.WithContext(proto.WithUserContext(ctx, other), access.ReadWriteAccess)

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks",
		strings.NewReader(`{"path":"a.bin","ref":{"name":"refs/heads/main"}}`))
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Content-Type", lfs.MediaType)
	w := httptest.NewRecorder()
	serviceLfsLocks(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body)
	}
	var resp lfs.LockResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Lock.ID == "" || resp.Lock.Path != "a.bin" || resp.Lock.Owner.Name != "owner" || resp.Lock.LockedAt.IsZero() {
		t.Errorf("conflict lock = %+v, want a.bin held by owner", resp.Lock)
	}
}

// TestLFSLockForceUnlockWrapsLock checks that a force unlock answers
// {"lock": ...} like an ordinary unlock.
func TestLFSLockForceUnlockWrapsLock(t *testing.T) {
	ctx := newLocksParamsContext(t)
	dbx := db.FromContext(ctx)
	repo := proto.RepositoryFromContext(ctx)
	lock, err := store.FromContext(ctx).GetLFSLockForPath(ctx, dbx, repo.ID(), "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := backend.FromContext(ctx).CreateUser(ctx, "admin2", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx = access.WithContext(proto.WithUserContext(ctx, admin), access.AdminAccess)

	id := strconv.FormatInt(lock.ID, 10)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/"+id+"/unlock",
		strings.NewReader(`{"force":true}`))
	req.Header.Set("Accept", lfs.MediaType)
	req.Header.Set("Content-Type", lfs.MediaType)
	req = mux.SetURLVars(req, map[string]string{"lock_id": id})
	w := httptest.NewRecorder()
	serviceLfsLocksDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
	}
	var resp lfs.LockResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Lock.ID != id || resp.Lock.Path != "a.bin" {
		t.Errorf("lock = %+v, want id %s path a.bin", resp.Lock, id)
	}
}

// TestLFSLocksAnonymousWriterAsksForCredentials checks that an anonymous
// caller with write access (anon-access: read-write) is asked to sign in
// before creating or deleting a lock, since a lock needs an owner. Create
// used to answer 404 "user not found" and unlock a 401 without a challenge,
// so git-lfs never offered credentials.
func TestLFSLocksAnonymousWriterAsksForCredentials(t *testing.T) {
	ctx := newLocksParamsContext(t)
	repo := proto.RepositoryFromContext(ctx)
	ctx = access.WithContext(proto.WithUserContext(ctx, nil), access.ReadWriteAccess)
	ctx = proto.WithRepositoryContext(ctx, repo)

	create := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks",
		strings.NewReader(`{"path":"c.bin","ref":{"name":"refs/heads/main"}}`))
	unlock := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/1/unlock",
		strings.NewReader(`{"force":false}`))
	unlock = mux.SetURLVars(unlock, map[string]string{"lock_id": "1"})

	for name, rq := range map[string]struct {
		req     *http.Request
		handler http.HandlerFunc
	}{
		"create": {create, serviceLfsLocks},
		"unlock": {unlock, serviceLfsLocksDelete},
	} {
		t.Run(name, func(t *testing.T) {
			rq.req.Header.Set("Accept", lfs.MediaType)
			rq.req.Header.Set("Content-Type", lfs.MediaType)
			w := httptest.NewRecorder()
			rq.handler(w, rq.req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body)
			}
			if w.Header().Get("LFS-Authenticate") == "" || w.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("no credentials challenge: %v", w.Header())
			}
		})
	}
}
