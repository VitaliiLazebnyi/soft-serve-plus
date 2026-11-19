package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
)

// storePasswordHash writes a bcrypt hash of password into username's row,
// as a database from an older version may hold one. Nothing sets a password
// any more, and no login reads it.
func storePasswordHash(t *testing.T, ctx context.Context, username, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	dbx := db.FromContext(ctx)
	res, err := dbx.ExecContext(ctx, dbx.Rebind("UPDATE users SET password = ? WHERE username = ?"), string(hash), username)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("set password of %s: %d rows, %v", username, n, err)
	}
}

// serveRoute calls a git route handler as withAccess does once it lets a
// request through, for tests that drive a handler directly. The caller,
// repository and access level are those the test put in r's context; the
// repository name, directory and service come from r's URL variables, or
// from the repository.
func serveRoute(h gitHandler, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vars := mux.Vars(r)
	gr := &gitRequest{
		name:    vars["repo"],
		dir:     vars["dir"],
		service: git.Service(vars["service"]),
		user:    proto.UserFromContext(ctx),
		repo:    proto.RepositoryFromContext(ctx),
		level:   access.FromContext(ctx),
	}
	if gr.name == "" && gr.repo != nil {
		gr.name = gr.repo.Name()
	}
	h(newGitServer(ctx, nil), w, r, gr)
}

// gitRouteFor returns the git route with path.
func gitRouteFor(t *testing.T, path string) gitRoute {
	t.Helper()
	for _, rt := range gitRoutes {
		if rt.path == path {
			return rt
		}
	}
	t.Fatalf("no git route %q", path)
	return gitRoute{}
}

// testRouter returns the HTTP router for ctx. The removals of repositories
// its pushes create stop when the test ends.
func testRouter(t *testing.T, ctx context.Context) http.Handler {
	t.Helper()
	pending := newPushCreates()
	t.Cleanup(func() { _ = pending.close(context.Background()) })
	return newRouter(ctx, pending)
}

// fixtureAdmin returns the admin account the first migration creates, the
// owner of fixture repositories: the backend refuses a create by a caller
// who may not create, as an anonymous caller may not by default.
func fixtureAdmin(t testing.TB, be *backend.Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}
