package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
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
		"cursor=1x",
		"cursor=99999999999999999999",
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
		"":                           {locks: 2},
		"limit=1":                    {locks: 1, cursor: "1"},
		"cursor=1&limit=1":           {locks: 1, cursor: "2"},
		"cursor=2&limit=1":           {locks: 0},
		"limit=1000":                 {locks: 2},
		"cursor=1&limit=0":           {locks: 1},
		"cursor=9223372036854775807": {locks: 0},
		"path=a.bin":                 {locks: 1},
		"path=a.bin&id=":             {locks: 1},
		"cursor=&limit=":             {locks: 2},
		"limit=1&cursor=01":          {locks: 1, cursor: "2"},
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
		`{"cursor":"99999999999999999999"}`,
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

	w := verify(`{"cursor":"1","limit":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
	}
	var resp lfs.LockVerifyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Ours) != 1 || len(resp.Theirs) != 0 || resp.NextCursor != "2" {
		t.Errorf("got ours %d, theirs %d, next_cursor %q; want 1, 0, \"2\"", len(resp.Ours), len(resp.Theirs), resp.NextCursor)
	}
}

// TestLFSLocksPagingIsStable pages through the lock list and the verify
// endpoint by next_cursor while locks are created and deleted between pages,
// all of them in the same second. Every lock that exists throughout must be
// listed exactly once. Pages used to be taken by row offset over the
// one-second updated_at, so ties came back in no fixed order and a lock
// created or deleted between pages shifted the others: some were listed
// twice and others never, and verify could leave out another user's lock.
func TestLFSLocksPagingIsStable(t *testing.T) {
	ctx := newLocksParamsContext(t)
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	repo := proto.RepositoryFromContext(ctx)
	owner := proto.UserFromContext(ctx)

	is := is.New(t)
	const n = 30
	for i := range n {
		is.NoErr(datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "f"+strconv.Itoa(i)+".bin", "refs/heads/main"))
	}
	if _, err := dbx.ExecContext(ctx, `UPDATE lfs_locks SET updated_at = '2026-01-01 00:00:00';`); err != nil {
		t.Fatalf("update: %v", err)
	}

	// between deletes the first lock listed, first, after the first page and
	// adds a new lock after the second.
	between := func(t *testing.T, page int, first string) {
		t.Helper()
		switch page {
		case 1:
			lock, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), first)
			if err != nil {
				t.Fatalf("get %s: %v", first, err)
			}
			if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lock.ID); err != nil {
				t.Fatalf("delete %s: %v", first, err)
			}
		case 2:
			if err := datastore.CreateLFSLockForUser(ctx, dbx, repo.ID(), owner.ID(), "new.bin", "refs/heads/main"); err != nil {
				t.Fatalf("create new.bin: %v", err)
			}
		}
	}
	// The locks that exist throughout; a.bin and b.bin come first, and one of
	// them is deleted between pages.
	var want []string
	for i := range n {
		want = append(want, "f"+strconv.Itoa(i)+".bin")
	}
	check := func(t *testing.T, seen map[string]int) {
		t.Helper()
		for _, path := range want {
			if seen[path] != 1 {
				t.Errorf("%s listed %d times, want 1", path, seen[path])
			}
		}
		if seen["new.bin"] > 1 {
			t.Errorf("new.bin listed %d times, want at most 1", seen["new.bin"])
		}
	}

	t.Run("list", func(t *testing.T) {
		seen := map[string]int{}
		var cursor, first string
		for page := 1; page <= 2*n; page++ {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/repo.git/info/lfs/locks?limit=4&cursor="+cursor, nil)
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
			for _, l := range resp.Locks {
				seen[l.Path]++
			}
			if first == "" && len(resp.Locks) > 0 {
				first = resp.Locks[0].Path
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
			between(t, page, first)
		}
		check(t, seen)
	})

	// Drop the list subtest's new lock so verify can add it again.
	lock, err := datastore.GetLFSLockForPath(ctx, dbx, repo.ID(), "new.bin")
	if err != nil {
		t.Fatalf("get new.bin: %v", err)
	}
	if err := datastore.DeleteLFSLock(ctx, dbx, repo.ID(), lock.ID); err != nil {
		t.Fatalf("delete new.bin: %v", err)
	}
	if _, err := dbx.ExecContext(ctx, `UPDATE lfs_locks SET updated_at = '2026-01-01 00:00:00';`); err != nil {
		t.Fatalf("update: %v", err)
	}

	t.Run("verify", func(t *testing.T) {
		seen := map[string]int{}
		var cursor, first string
		for page := 1; page <= 2*n; page++ {
			body := fmt.Sprintf(`{"cursor":%q,"limit":4}`, cursor)
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/locks/verify", strings.NewReader(body))
			req.Header.Set("Content-Type", lfs.MediaType)
			req.Header.Set("Accept", lfs.MediaType)
			w := httptest.NewRecorder()
			serviceLfsLocksVerify(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
			}
			var resp lfs.LockVerifyResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			locks := slices.Concat(resp.Ours, resp.Theirs)
			for _, l := range locks {
				seen[l.Path]++
			}
			if first == "" && len(locks) > 0 {
				first = locks[0].Path
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
			between(t, page, first)
		}
		check(t, seen)
	})
}
