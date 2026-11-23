package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

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
