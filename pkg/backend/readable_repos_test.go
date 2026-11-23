package backend

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestReadableRepositories verifies that the bulk listing, which loads the
// caller's collaborations once, picks exactly the repositories Readable
// lets the caller read one by one.
func TestReadableRepositories(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range []struct {
		name    string
		private bool
	}{
		{"public", false},
		{"secret", true},
		{"shared", true},
		{"shared-public", false},
		{"blocked", true},
	} {
		_, err := be.CreateRepository(ctx, r.name, owner, proto.RepositoryOptions{Private: r.private})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := be.AddCollaborator(ctx, "shared", "collab", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ctx, "shared-public", "collab", access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.AddCollaborator(ctx, "blocked", "collab", access.NoAccess); err != nil {
		t.Fatal(err)
	}

	// Rows whose directories are gone: Readable refuses them to every
	// non-admin, owner and collaborators included, so the listing must too.
	for _, r := range []struct {
		name    string
		private bool
	}{
		{"gone-public", false},
		{"gone-secret", true},
	} {
		_, err := be.CreateRepository(ctx, r.name, owner, proto.RepositoryOptions{Private: r.private})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := be.AddCollaborator(ctx, "gone-secret", "collab", access.ReadOnlyAccess); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(be.repoPath("gone-public")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(be.repoPath("gone-secret")); err != nil {
		t.Fatal(err)
	}

	names := func(repos []proto.Repository) map[string]bool {
		m := make(map[string]bool, len(repos))
		for _, r := range repos {
			m[r.Name()] = true
		}
		return m
	}

	for _, anon := range []access.Level{access.NoAccess, access.ReadOnlyAccess} {
		if err := be.SetAnonAccess(ctx, anon); err != nil {
			t.Fatal(err)
		}
		for _, user := range []proto.User{nil, owner, collab, stranger, admin} {
			repos, err := be.ReadableRepositories(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			got := names(repos)

			all, err := be.Repositories(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, r := range all {
				// Admins are listed every row, directory or not, as the
				// per-name access check listed them before.
				if user != nil && user.IsAdmin() {
					want[r.Name()] = true
					continue
				}
				if rr, _ := noLookupError(t)(be.Readable(ctx, r.Name(), user)); rr != nil {
					want[r.Name()] = true
				}
			}
			if got, want := got, want; !reflect.DeepEqual(got, want) {
				t.Fatalf("got = %v, want %v", got, want)
			}
		}
	}

	// Spot checks so the comparison above cannot pass on two empty lists.
	repos, err := be.ReadableRepositories(ctx, collab)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(repos), (map[string]bool{"public": true, "shared": true, "shared-public": true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("names(repos) = %v, want %v", got, want)
	}
	repos, err = be.ReadableRepositories(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 5 {
		t.Fatalf("len(repos) = %v, want %v", got, 5)
	}

	// No non-admin is listed a row whose directory is gone.
	for _, anon := range []access.Level{access.NoAccess, access.ReadOnlyAccess} {
		if err := be.SetAnonAccess(ctx, anon); err != nil {
			t.Fatal(err)
		}
		for _, user := range []proto.User{nil, owner, collab, stranger} {
			repos, err := be.ReadableRepositories(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			got := names(repos)
			if got["gone-public"] {
				t.Fatal("not true: !got[\"gone-public\"]")
			}
			if got["gone-secret"] {
				t.Fatal("not true: !got[\"gone-secret\"]")
			}
		}
	}
	repos, err = be.ReadableRepositories(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	got := names(repos)
	if !(got["gone-public"] && got["gone-secret"]) {
		t.Fatal("not true: got[\"gone-public\"] && got[\"gone-secret\"]")
	}
}
