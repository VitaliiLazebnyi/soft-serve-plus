package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// routeNeed is what a git or LFS route asks of a caller for one method.
type routeNeed int

const (
	// notAllowed: the method is refused with 405 before anything else.
	notAllowed routeNeed = iota
	// needRead: a caller who may read the repository is served.
	needRead
	// needWrite: a caller who may only read is refused.
	needWrite
	// preflight: answered by the CORS middleware before any route, the
	// same for every caller.
	preflight
)

func (n routeNeed) String() string {
	return [...]string{"405", "read", "write", "cors"}[n]
}

// routeWant is the classification of one route and method.
type routeWant struct {
	need routeNeed
	// lfs routes answer refusals with an LFS JSON body, others with text.
	lfs bool
	// creates is whether a caller who may create repositories creates a
	// missing one by this request (push-to-create).
	creates bool
}

// TestRouteAccessTable pins, by requests through the router, the access
// level every git and LFS route needs for every method, whether its refusals
// are LFS JSON, and which requests create a missing repository. It is a
// black-box test on purpose: it describes the behaviour, not the code that
// decides it, so it holds however the route table is written.
func TestRouteAccessTable(t *testing.T) {
	f := newOracleFixture(t)
	is := is.New(t)

	// "pub" is public: anonymous callers and "stranger" may read it, and
	// only its owner may write. "secret" is private to its owner.
	_, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	ownerCtx := proto.WithUserContext(f.ctx, f.owner)
	is.NoErr(f.be.AddCollaborator(ownerCtx, "pub", "stranger", access.ReadOnlyAccess))
	writer := f.ownerAuth(t)
	reader := "Token " + f.token
	// A user who may create repositories, and can read nothing of the
	// owner's.
	creator, err := f.be.CreateUser(f.ctx, "creator", proto.UserOptions{})
	is.NoErr(err)
	creatorToken, err := f.be.CreateAccessToken(f.ctx, creator, "c", time.Now().Add(time.Hour))
	is.NoErr(err)

	oid := strings.Repeat("a", 64)
	read := routeWant{need: needRead}
	write := routeWant{need: needWrite}
	lfsRead := routeWant{need: needRead, lfs: true}
	lfsWrite := routeWant{need: needWrite, lfs: true}
	push := routeWant{need: needWrite, creates: true}
	no := routeWant{need: notAllowed}
	noLFS := routeWant{need: notAllowed, lfs: true}

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
	routes := []struct {
		path string // under /<repo>.git
		want map[string]routeWant
	}{
		{"/git-upload-pack", map[string]routeWant{http.MethodPost: read}},
		{"/git-upload-archive", map[string]routeWant{http.MethodPost: read}},
		{"/git-receive-pack", map[string]routeWant{http.MethodPost: push}},
		{"/info/refs?service=git-upload-pack", map[string]routeWant{http.MethodGet: read, http.MethodHead: read}},
		// HEAD answers like GET but does not create the repository.
		{"/info/refs?service=git-receive-pack", map[string]routeWant{http.MethodGet: push, http.MethodHead: write}},
		{"/info/lfs/objects/batch", map[string]routeWant{http.MethodPost: lfsRead}},
		{"/info/lfs/objects/basic/" + oid, map[string]routeWant{http.MethodGet: lfsRead, http.MethodHead: lfsRead, http.MethodPut: lfsWrite}},
		{"/info/lfs/objects/basic/verify", map[string]routeWant{http.MethodPost: lfsRead}},
		{"/info/lfs/locks", map[string]routeWant{http.MethodGet: lfsRead, http.MethodHead: lfsRead, http.MethodPost: lfsWrite}},
		{"/info/lfs/locks/verify", map[string]routeWant{http.MethodPost: lfsWrite}},
		{"/info/lfs/locks/1/unlock", map[string]routeWant{http.MethodPost: lfsWrite}},
	}

	// denied reports whether the authorization middleware refused the
	// request. Handlers answer the empty requests sent here with 2xx, 404,
	// 415 or 422, never 401 or 403.
	denied := func(code int) bool {
		return code == http.StatusUnauthorized || code == http.StatusForbidden
	}

	fresh := 0
	for _, rt := range routes {
		for _, method := range methods {
			want, ok := rt.want[method]
			if !ok {
				want = no
				if strings.HasPrefix(rt.path, "/info/lfs") {
					want = noLFS
				}
			}
			if method == http.MethodOptions {
				want = routeWant{need: preflight, lfs: want.lfs}
			}
			name := method + " " + rt.path
			at := func(repo string) string { return "/" + repo + ".git" + rt.path }

			var got routeWant
			got.lfs = want.lfs // checked below where a refusal has a body
			switch w := f.do(method, at("pub"), writer); {
			case method == http.MethodOptions && w.Code == http.StatusOK && w.Body.Len() == 0:
				got.need = preflight
				for _, auth := range []string{"", reader} {
					if a := f.do(method, at("secret"), auth); a.Code != http.StatusOK || a.Body.Len() != 0 {
						t.Errorf("%s: 200 for the owner, %d %q for another caller", name, a.Code, a.Body)
					}
				}
			case w.Code == http.StatusMethodNotAllowed:
				got.need = notAllowed
				// Every caller gets the same 405.
				for _, auth := range []string{"", reader} {
					if c := f.do(method, at("secret"), auth).Code; c != http.StatusMethodNotAllowed {
						t.Errorf("%s: 405 for the owner, %d for another caller", name, c)
					}
				}
			case denied(w.Code):
				t.Errorf("%s: the owner was refused with %d", name, w.Code)
				continue
			default:
				if denied(f.do(method, at("pub"), reader).Code) {
					got.need = needWrite
					// An anonymous caller is asked for credentials.
					a := f.do(method, at("pub"), "")
					if a.Code != http.StatusUnauthorized || a.Header().Get("WWW-Authenticate") == "" {
						t.Errorf("%s: anonymous reader got %d, want 401 with a challenge", name, a.Code)
					}
				} else {
					got.need = needRead
					if c := f.do(method, at("pub"), "").Code; denied(c) {
						t.Errorf("%s: anonymous reader of a public repo refused with %d", name, c)
					}
				}
				// Unreadable: refused, as JSON on LFS routes.
				a := f.do(method, at("secret"), "")
				if a.Code != http.StatusUnauthorized {
					t.Errorf("%s: anonymous on a private repo got %d, want 401", name, a.Code)
				}
				got.lfs = strings.HasPrefix(a.Header().Get("Content-Type"), "application/vnd.git-lfs+json")
				if !got.lfs && !strings.HasPrefix(a.Header().Get("Content-Type"), "text/plain") {
					t.Errorf("%s: refusal has Content-Type %q", name, a.Header().Get("Content-Type"))
				}
			}

			// Push-to-create: only a creator's push requests create.
			fresh++
			repo := fmt.Sprintf("fresh%d", fresh)
			f.do(method, at(repo), "Token "+creatorToken)
			_, err := f.be.Repository(f.ctx, repo)
			got.creates = err == nil

			if got != want {
				t.Errorf("%s: got {%v lfs=%v creates=%v}, want {%v lfs=%v creates=%v}",
					name, got.need, got.lfs, got.creates, want.need, want.lfs, want.creates)
			}
		}
	}
}

// TestRouteAccessAnonymousWriter pins what an anonymous caller gets on the
// write routes of a public repository when anonymous users may write
// (anon-access read-write): authorization lets every request through, so
// the answer is the handler's. Under anon-access read-only the same
// requests are refused by authorization with 401. Creating or deleting a
// lock then asks for credentials in the handler, since a lock needs an
// owner.
func TestRouteAccessAnonymousWriter(t *testing.T) {
	f := newOracleFixture(t)
	is := is.New(t)
	pub, err := f.be.CreateRepository(f.ctx, "pub", f.owner, proto.RepositoryOptions{})
	is.NoErr(err)
	// A lock of the owner's, which verify lists for others as theirs.
	_, err = store.FromContext(f.ctx).CreateLFSLockForUser(f.ctx, db.FromContext(f.ctx), pub.ID(), f.owner.ID(), "owned.bin", "refs/heads/main")
	is.NoErr(err)

	oid := sha256Hex("x")
	lfsJSON := map[string]string{"Accept": lfs.MediaType, "Content-Type": lfs.MediaType}
	octets := map[string]string{"Content-Type": "application/octet-stream"}
	rpc := map[string]string{"Content-Type": "application/x-git-receive-pack-request"}
	rows := []struct {
		method, path string
		hdr          map[string]string
		body         string
		// want is the status with anon-access read-write.
		want int
		// challenge is whether that answer asks for credentials.
		challenge bool
	}{
		{http.MethodGet, "/info/refs?service=git-receive-pack", nil, "", http.StatusOK, false},
		{http.MethodHead, "/info/refs?service=git-receive-pack", nil, "", http.StatusOK, false},
		{http.MethodPost, "/git-receive-pack", nil, "", http.StatusUnsupportedMediaType, false},
		{http.MethodPost, "/git-receive-pack", rpc, "0000", http.StatusOK, false},
		{http.MethodPut, "/info/lfs/objects/basic/" + oid, nil, "x", http.StatusUnsupportedMediaType, false},
		{http.MethodPut, "/info/lfs/objects/basic/" + oid, octets, "x", http.StatusOK, false},
		{http.MethodPost, "/info/lfs/locks", lfsJSON, "{", http.StatusBadRequest, false},
		{http.MethodPost, "/info/lfs/locks", lfsJSON, `{"path":"a.bin"}`, http.StatusUnauthorized, true},
		// Verify answers; see the body check below.
		{http.MethodPost, "/info/lfs/locks/verify", lfsJSON, `{}`, http.StatusOK, false},
		{http.MethodPost, "/info/lfs/locks/1/unlock", lfsJSON, `{}`, http.StatusUnauthorized, true},
	}

	do := func(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		return f.send(method, "/pub.git"+path, hdr, body)
	}

	// anon-access read-only: authorization refuses each, asking for
	// credentials.
	is.NoErr(f.be.SetAnonAccess(f.ctx, access.ReadOnlyAccess))
	for _, row := range rows {
		w := do(row.method, row.path, row.hdr, row.body)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("read-only: %s %s: got %d, want 401 with a challenge", row.method, row.path, w.Code)
		}
	}

	is.NoErr(f.be.SetAnonAccess(f.ctx, access.ReadWriteAccess))
	for _, row := range rows {
		w := do(row.method, row.path, row.hdr, row.body)
		if w.Code != row.want || (w.Header().Get("WWW-Authenticate") != "") != row.challenge {
			t.Errorf("read-write: %s %s %q: got %d (challenge %q), want %d (challenge %v): %s",
				row.method, row.path, row.body, w.Code, w.Header().Get("WWW-Authenticate"), row.want, row.challenge, w.Body)
		}
	}

	// The anonymous writer owns no lock: the owner's comes back as theirs.
	w := do(http.MethodPost, "/info/lfs/locks/verify", lfsJSON, `{}`)
	is.Equal(w.Code, http.StatusOK)
	var verify lfs.LockVerifyResponse
	is.NoErr(json.Unmarshal(w.Body.Bytes(), &verify))
	is.Equal(len(verify.Ours), 0)
	is.Equal(len(verify.Theirs), 1)
	is.Equal(verify.Theirs[0].Path, "owned.bin")
	is.Equal(verify.Theirs[0].Owner.Name, f.owner.Username())
}
