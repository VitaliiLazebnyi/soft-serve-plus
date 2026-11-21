package web

import (
	"net/http"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/git"
)

// TestAuthorize pins the decision order for every kind of caller against
// every kind of route and repository state. The property that matters for
// the existence oracle is that, for a caller who cannot read, the verdict
// for a private repository (!readable, NoAccess) equals the verdict for a
// missing one (!readable, whatever level the caller gets for a missing
// name), so the level alone must not change the answer.
func TestAuthorize(t *testing.T) {
	t.Parallel()
	type route struct {
		name string
		need routeAccess
	}
	isLFS := func(rt route) bool { return rt.need.lfs }
	infoRefs := gitRouteFor(t, "/info/refs")
	rpc := gitRouteFor(t, "/{service:(?:git-upload-archive|git-upload-pack|git-receive-pack)$}")
	basic := gitRouteFor(t, "/info/lfs/objects/basic/{oid:[0-9a-f]{64}$}")
	routes := []route{
		{"fetch advert", infoRefs.access(http.MethodGet, git.UploadPackService)},
		{"fetch rpc", rpc.access(http.MethodPost, git.UploadPackService)},
		{"push advert", infoRefs.access(http.MethodGet, git.ReceivePackService)},
		{"push rpc", rpc.access(http.MethodPost, git.ReceivePackService)},
		{"lfs batch", gitRouteFor(t, "/info/lfs/objects/batch").access(http.MethodPost, "")},
		{"lfs upload", basic.access(http.MethodPut, "")},
		{"lfs lock", gitRouteFor(t, "/info/lfs/locks").access(http.MethodPost, "")},
		{"push advert head", infoRefs.access(http.MethodHead, git.ReceivePackService)},
	}

	type caller struct {
		name      string
		anonymous bool
		badCreds  bool
		// level the caller gets for a missing repository
		missing access.AccessLevel
	}
	callers := []caller{
		{"anonymous", true, false, access.ReadOnlyAccess},
		{"anonymous no-access", true, false, access.NoAccess},
		{"bad credentials", true, true, access.ReadOnlyAccess},
		{"signed-in", false, false, access.ReadWriteAccess},
	}

	// Missing and private must be indistinguishable for every caller who
	// cannot read the private repository.
	for _, c := range callers {
		for _, rt := range routes {
			private := authorize(rt.need, false, access.NoAccess, c.anonymous, c.badCreds)
			missing := authorize(rt.need, false, c.missing, c.anonymous, c.badCreds)
			want := denyUnreadable
			switch {
			case c.badCreds && !isLFS(rt):
				// LFS renders bad credentials from denyRead so the body
				// is JSON; the verdict stays denyUnreadable.
				want = forbid
			case rt.need.create && !c.anonymous:
				// The one inherent difference: a push to a new name by a
				// user who may create repositories creates it.
				if missing != createThenServe {
					t.Errorf("%s/%s: missing: got %v, want createThenServe", c.name, rt.name, missing)
				}
				if private != denyUnreadable {
					t.Errorf("%s/%s: private: got %v, want denyUnreadable", c.name, rt.name, private)
				}
				continue
			}
			if private != want || missing != want {
				t.Errorf("%s/%s: private=%v missing=%v, want both %v", c.name, rt.name, private, missing, want)
			}
		}
	}

	// Readers and writers of an existing repository.
	tests := []struct {
		name      string
		route     route
		level     access.AccessLevel
		anonymous bool
		want      verdict
	}{
		{"anon reader fetch", routes[0], access.ReadOnlyAccess, true, serve},
		{"anon reader push", routes[2], access.ReadOnlyAccess, true, unauthorized},
		{"anon reader push rpc", routes[3], access.ReadOnlyAccess, true, unauthorized},
		{"anon reader lfs batch", routes[4], access.ReadOnlyAccess, true, serve},
		{"anon reader lfs upload", routes[5], access.ReadOnlyAccess, true, unauthorizedLFS},
		{"anon reader lfs lock", routes[6], access.ReadOnlyAccess, true, unauthorizedLFS},
		{"reader fetch", routes[0], access.ReadOnlyAccess, false, serve},
		{"reader push", routes[2], access.ReadOnlyAccess, false, forbid},
		{"reader push rpc", routes[3], access.ReadOnlyAccess, false, forbid},
		{"reader lfs upload", routes[5], access.ReadOnlyAccess, false, forbidLFSWrite},
		{"writer push", routes[2], access.ReadWriteAccess, false, serve},
		{"writer push rpc", routes[3], access.ReadWriteAccess, false, serve},
		{"writer lfs upload", routes[5], access.ReadWriteAccess, false, serve},
		{"writer lfs lock", routes[6], access.ReadWriteAccess, false, serve},
		{"anon writer push", routes[2], access.ReadWriteAccess, true, serve},
		{"writer push head", routes[7], access.ReadWriteAccess, false, serve},
	}
	for _, tt := range tests {
		got := authorize(tt.route.need, true, tt.level, tt.anonymous, false)
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}

	// Bad credentials on a readable repository are refused, and never
	// create anything even when the anonymous level would allow a push.
	for _, rt := range routes {
		if got := authorize(rt.need, true, access.ReadOnlyAccess, true, true); got != forbid {
			t.Errorf("bad creds on readable %s: got %v, want forbid", rt.name, got)
		}
		want := forbid
		if isLFS(rt) {
			want = denyUnreadable
		}
		if got := authorize(rt.need, false, access.ReadWriteAccess, true, true); got != want {
			t.Errorf("bad creds on missing %s: got %v, want %v", rt.name, got, want)
		}
	}

	// A push may only create from the push endpoints, and not on HEAD.
	if got := authorize(routeAccess{write: true}, false, access.ReadWriteAccess, false, false); got != denyUnreadable {
		t.Errorf("write without create: got %v, want denyUnreadable", got)
	}
	for _, rt := range gitRoutes {
		for _, method := range rt.allowed() {
			for _, service := range []git.Service{"", git.UploadPackService, git.UploadArchiveService, git.ReceivePackService} {
				need := rt.access(method, service)
				wantCreate := rt.service && service == git.ReceivePackService && method != http.MethodHead
				if need.create != wantCreate {
					t.Errorf("%s %s %q: create = %v, want %v", method, rt.path, service, need.create, wantCreate)
				}
			}
		}
	}
}
