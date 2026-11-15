package web

import (
	"net/http"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/gorilla/mux"
)

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
func gitRouteFor(t *testing.T, path string) GitRoute {
	t.Helper()
	for _, rt := range gitRoutes {
		if rt.path == path {
			return rt
		}
	}
	t.Fatalf("no git route %q", path)
	return GitRoute{}
}
