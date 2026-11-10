package backend

import (
	"context"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
)

// TestReadableRepositories verifies that the bulk listing, which loads the
// caller's collaborations once, picks exactly the repositories Readable
// lets the caller read one by one.
func TestReadableRepositories(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)
	ctx = db.WithContext(ctx, be.db)
	ctx = store.WithContext(ctx, be.store)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

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
		is.NoErr(err)
	}
	is.NoErr(be.AddCollaborator(ctx, "shared", "collab", access.ReadOnlyAccess))
	is.NoErr(be.AddCollaborator(ctx, "shared-public", "collab", access.ReadWriteAccess))
	is.NoErr(be.AddCollaborator(ctx, "blocked", "collab", access.NoAccess))

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
		is.NoErr(err)
	}
	is.NoErr(be.AddCollaborator(ctx, "gone-secret", "collab", access.ReadOnlyAccess))
	is.NoErr(os.RemoveAll(be.repoPath("gone-public")))
	is.NoErr(os.RemoveAll(be.repoPath("gone-secret")))

	names := func(repos []proto.Repository) map[string]bool {
		m := make(map[string]bool, len(repos))
		for _, r := range repos {
			m[r.Name()] = true
		}
		return m
	}

	for _, anon := range []access.AccessLevel{access.NoAccess, access.ReadOnlyAccess} {
		is.NoErr(be.SetAnonAccess(ctx, anon))
		for _, user := range []proto.User{nil, owner, collab, stranger, admin} {
			repos, err := be.ReadableRepositories(ctx, user)
			is.NoErr(err)
			got := names(repos)

			all, err := be.Repositories(ctx)
			is.NoErr(err)
			want := map[string]bool{}
			for _, r := range all {
				// Admins are listed every row, directory or not, as the
				// per-name access check listed them before.
				if user != nil && user.IsAdmin() {
					want[r.Name()] = true
					continue
				}
				if rr, _ := be.Readable(ctx, r.Name(), user); rr != nil {
					want[r.Name()] = true
				}
			}
			is.Equal(got, want)
		}
	}

	// Spot checks so the comparison above cannot pass on two empty lists.
	repos, err := be.ReadableRepositories(ctx, collab)
	is.NoErr(err)
	is.Equal(names(repos), map[string]bool{"public": true, "shared": true, "shared-public": true})
	repos, err = be.ReadableRepositories(ctx, owner)
	is.NoErr(err)
	is.Equal(len(repos), 5)

	// No non-admin is listed a row whose directory is gone.
	for _, anon := range []access.AccessLevel{access.NoAccess, access.ReadOnlyAccess} {
		is.NoErr(be.SetAnonAccess(ctx, anon))
		for _, user := range []proto.User{nil, owner, collab, stranger} {
			repos, err := be.ReadableRepositories(ctx, user)
			is.NoErr(err)
			got := names(repos)
			is.True(!got["gone-public"])
			is.True(!got["gone-secret"])
		}
	}
	repos, err = be.ReadableRepositories(ctx, admin)
	is.NoErr(err)
	got := names(repos)
	is.True(got["gone-public"] && got["gone-secret"])
}
