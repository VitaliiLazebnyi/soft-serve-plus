package backend

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestAccessLevelCollaboratorPrivateRepo verifies that the anonymous access
// floor does not raise a collaborator's explicit level on a private
// repository, while still applying on public repositories.
func TestAccessLevelCollaboratorPrivateRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		private bool
		anon    access.Level
		collab  access.Level
		want    access.Level
	}{
		{"private no-access collab, anon read-only", true, access.ReadOnlyAccess, access.NoAccess, access.NoAccess},
		{"private read-only collab, anon read-write", true, access.ReadWriteAccess, access.ReadOnlyAccess, access.ReadOnlyAccess},
		{"private read-write collab, anon read-only", true, access.ReadOnlyAccess, access.ReadWriteAccess, access.ReadWriteAccess},
		{"public no-access collab, anon read-only", false, access.ReadOnlyAccess, access.NoAccess, access.ReadOnlyAccess},
		{"public read-only collab, anon read-write", false, access.ReadWriteAccess, access.ReadOnlyAccess, access.ReadWriteAccess},
		{"public no-access collab, anon no-access", false, access.NoAccess, access.NoAccess, access.NoAccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			be, _ := newTestBackend(t)
			ctx := context.Background()

			if err := be.SetAnonAccess(ctx, tt.anon); err != nil {
				t.Fatal(err)
			}

			_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{Private: tt.private})
			if err != nil {
				t.Fatal(err)
			}

			user, err := be.CreateUser(ctx, "collab", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
				return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", tt.collab)
			}); err != nil {
				t.Fatal(err)
			}

			if got := levelOf(t, be, ctx, "repo", user); got != tt.want {
				t.Fatalf("levelOf(t, be, ctx, \"repo\", user) = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAccessLevelPrivateRepoNonCollaborator verifies that a signed-in user
// who is not a collaborator has no access to a private repository.
func TestAccessLevelPrivateRepoNonCollaborator(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	user, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "repo", user); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", user) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "repo", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, access.NoAccess)
	}
}

// TestAccessLevelPublicRepoNonCollaborator verifies that the anonymous
// level is a floor for a registered non-collaborator on a public repository,
// as it is for collaborators: a registered user never gets less than an
// anonymous caller. Registered users keep read-only when the anonymous level
// is lower.
func TestAccessLevelPublicRepoNonCollaborator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		anon access.Level
		want access.Level
	}{
		{"anon no-access", access.NoAccess, access.ReadOnlyAccess},
		{"anon read-only", access.ReadOnlyAccess, access.ReadOnlyAccess},
		{"anon read-write", access.ReadWriteAccess, access.ReadWriteAccess},
		{"anon admin-access", access.AdminAccess, access.AdminAccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			be, _ := newTestBackend(t)
			ctx := context.Background()

			if err := be.SetAnonAccess(ctx, tt.anon); err != nil {
				t.Fatal(err)
			}

			_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}

			user, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}

			if got := levelOf(t, be, ctx, "repo", user); got != tt.want {
				t.Fatalf("levelOf(t, be, ctx, \"repo\", user) = %v, want %v", got, tt.want)
			}
			if got := levelOf(t, be, ctx, "repo", nil); got != tt.anon {
				t.Fatalf("levelOf(t, be, ctx, \"repo\", nil) = %v, want %v", got, tt.anon)
			}
		})
	}
}

// TestDeleteUserWithRepositories verifies that a user who owns
// repositories is not deleted until those repositories are gone, and that
// their repositories are left untouched by the refusal.
func TestDeleteUserWithRepositories(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)

	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, admin)

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "owned", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = be.DeleteUser(ctx, "owner")
	if !errors.Is(err, proto.ErrUserOwnsRepos) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserOwnsRepos)")
	}
	_, err = be.User(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteUser(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	_, err = be.User(ctx, "owner")
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}

	if !errors.Is(be.DeleteUser(ctx, "owner"), proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(be.DeleteUser(ctx, \"owner\"), proto.ErrUserNotFound)")
	}
}

// TestDeleteRepositoryWithoutDirectory verifies that a repository whose
// directory is gone can still be deleted, and that deleting a repository
// that does not exist at all reports it as not found.
func TestDeleteRepositoryWithoutDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)

	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "orphan", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(be.repoPath("orphan")); err != nil {
		t.Fatal(err)
	}

	if err := be.DeleteRepository(ctx, "orphan"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.DeleteRepository(ctx, "orphan"), proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(be.DeleteRepository(ctx, \"orphan\"), proto.ErrRepoNotFound)")
	}
}

// TestMutationsOnMissingUserOrKey verifies that changing a user who does not
// exist, or removing a key the user does not have, reports it instead of
// succeeding or leaking a raw SQL error.
func TestMutationsOnMissingUserOrKey(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)

	ctx := context.Background()
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx = proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	pk := newTestPublicKey(t)

	isErr := func(err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want %v", err, want)
		}
	}

	isErr(be.AddCollaborator(ctx, "repo", "ghost", access.ReadWriteAccess), proto.ErrUserNotFound)
	isErr(be.AddPublicKey(ctx, "ghost", pk), proto.ErrUserNotFound)
	isErr(be.RemovePublicKey(ctx, "ghost", pk), proto.ErrUserNotFound)
	isErr(be.RemovePublicKey(ctx, "alice", pk), proto.ErrPublicKeyNotFound)
	isErr(be.SetAdmin(ctx, "ghost", true), proto.ErrUserNotFound)
	isErr(be.SetUsername(ctx, "ghost", "phantom"), proto.ErrUserNotFound)

	// Nothing was created along the way.
	_, err = be.User(ctx, "phantom")
	isErr(err, proto.ErrUserNotFound)
	collabs, err := be.Collaborators(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(collabs); got != 0 {
		t.Fatalf("len(collabs) = %v, want %v", got, 0)
	}

	// The same changes on a user who exists still succeed.
	if err := be.AddPublicKey(ctx, "alice", pk); err != nil {
		t.Fatal(err)
	}
	if err := be.RemovePublicKey(ctx, "alice", pk); err != nil {
		t.Fatal(err)
	}
	if err := be.SetAdmin(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := be.SetUsername(ctx, "alice", "alicia"); err != nil {
		t.Fatal(err)
	}
}
