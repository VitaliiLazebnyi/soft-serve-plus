package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestLFSBatchActionsForwardOnlyJWT checks which credential a batch response
// copies into its actions. A Basic password or an access token sent with the
// batch request must not come back in the response body, where client traces
// and proxies can log it. A bearer JWT, which is how a client authenticated
// over SSH reaches the object URLs, is forwarded with its remaining lifetime
// in expires_in.
func TestLFSBatchActionsForwardOnlyJWT(t *testing.T) {
	is := is.New(t)
	ctx, be, datastore := newLFSTestContext(t)

	cfg := config.FromContext(ctx)
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "ssh", "host_key")
	_, err := keygen.New(cfg.SSH.KeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite())
	is.NoErr(err)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(be.SetPassword(ctx, "owner", "hunter2"))
	_, err = be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	is.NoErr(err)
	token, err := be.CreateAccessToken(ctx, owner, "t", time.Time{})
	is.NoErr(err)
	jwtToken := mintJWT(ctx, t, owner, "repo")

	// An object that exists, so a download batch returns an action.
	body := "payload"
	oid := "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" // sha256(body)
	repo, err := be.Repository(ctx, "repo")
	is.NoErr(err)
	objPath := filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10), "objects",
		filepath.FromSlash(lfs.Pointer{Oid: oid}.RelativePath()))
	is.NoErr(os.MkdirAll(filepath.Dir(objPath), 0o755))
	is.NoErr(os.WriteFile(objPath, []byte(body), 0o644))
	is.NoErr(datastore.CreateLFSObject(ctx, db.FromContext(ctx), repo.ID(), oid, int64(len(body))))

	router := lfsTestRouter(ctx)
	batch := func(op, auth string) lfs.BatchResponse {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/objects/batch",
			strings.NewReader(`{"operation":"`+op+`","objects":[{"oid":"`+oid+`","size":7}]}`))
		req.Header.Set("Accept", lfs.MediaType)
		req.Header.Set("Content-Type", lfs.MediaType)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s batch: got status %d: %s", op, w.Code, w.Body.String())
		}
		var resp lfs.BatchResponse
		is.NoErr(json.Unmarshal(w.Body.Bytes(), &resp))
		is.Equal(len(resp.Objects), 1)
		if resp.Objects[0].Error != nil {
			t.Fatalf("%s batch: object error: %+v", op, resp.Objects[0].Error)
		}
		return resp
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("owner:hunter2"))
	for _, auth := range []string{basic, "token " + token} {
		for _, op := range []string{lfs.OperationDownload, lfs.OperationUpload} {
			resp := batch(op, auth)
			is.True(len(resp.Objects[0].Actions) > 0)
			for name, action := range resp.Objects[0].Actions {
				if _, ok := action.Header["Authorization"]; ok {
					t.Errorf("%s %s action echoes the caller's %q credential", op, name, strings.Fields(auth)[0])
				}
				is.True(action.ExpiresIn == nil)
			}
		}
	}
	// The upload action keeps its Content-Type without a credential.
	up := batch(lfs.OperationUpload, basic).Objects[0].Actions[lfs.ActionUpload]
	is.Equal(up.Header["Content-Type"], "application/octet-stream")

	for _, op := range []string{lfs.OperationDownload, lfs.OperationUpload} {
		resp := batch(op, "Bearer "+jwtToken)
		is.True(len(resp.Objects[0].Actions) > 0)
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
	ctx, be, _ := newLFSTestContext(t)
	cfg := config.FromContext(ctx)
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "ssh", "host_key")
	if _, err := keygen.New(cfg.SSH.KeyPath, keygen.WithKeyType(keygen.Ed25519), keygen.WithWrite()); err != nil {
		t.Fatal(err)
	}
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	jwtToken := mintJWT(ctx, t, owner, "repo") // expires in an hour
	otherRepo := mintJWT(ctx, t, owner, "other")
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
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/repo.git/info/lfs/objects/batch", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if tc.now.IsZero() {
				tc.now = now
			}
			auth := lfsActionAuthFor(req, "repo", tc.now)
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
