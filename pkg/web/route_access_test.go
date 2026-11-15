package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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
)

func (n routeNeed) String() string {
	return [...]string{"405", "read", "write"}[n]
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

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
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
			name := method + " " + rt.path
			at := func(repo string) string { return "/" + repo + ".git" + rt.path }

			var got routeWant
			got.lfs = want.lfs // checked below where a refusal has a body
			switch w := f.do(method, at("pub"), writer); {
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
