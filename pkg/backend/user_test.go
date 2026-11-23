package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	charmlog "charm.land/log/v2"
	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
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

// With AdminOnlyCreate on, a missing name is read-only for non-admins and
// anonymous users, so nothing can be created there.
func TestAdminOnlyCreateFlagOnCapsBaseAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got := levelOf(t, be, ctx, "fresh", admin); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", admin) = %v, want %v", got, access.AdminAccess)
	}
	// Anonymous access is capped too: even when the server allows read-write
	// for anonymous users, a missing name is read-only.
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw
	if got := levelOf(t, be, ctx, "fresh", nil); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", nil) = %v, want %v", got, access.ReadOnlyAccess)
	}
}

// With AdminOnlyCreate off (the default), the backend keeps upstream
// behaviour: a registered user gets read-write on a missing name, so can
// create it.
func TestAdminOnlyCreateFlagOffKeepsUpstreamBaseAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadWriteAccess)
	}
	if got := levelOf(t, be, ctx, "fresh", nil); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", nil) = %v, want %v", got, access.ReadWriteAccess)
	}
}

// A database admin keeps admin access on a missing name whatever the flag.
func TestDatabaseAdminBaseAccessIgnoresFlag(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	pub := newTestPublicKey(t)
	root, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{pub}})
	if err != nil {
		t.Fatal(err)
	}

	cfg.AdminOnlyCreate = true
	if got := baseLevel(t, be, ctx, root); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, ctx, root) = %v, want %v", got, access.AdminAccess)
	}
	cfg.AdminOnlyCreate = false
	if got := baseLevel(t, be, ctx, root); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, ctx, root) = %v, want %v", got, access.AdminAccess)
	}
}

// With the flag off, a registered user on a missing name gets the anonymous
// level when that is higher than read-write (upstream behaviour), and the
// flag on caps it at read-only.
func TestAnonAdminLevelIsCappedOnlyWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	anonAdmin := access.AdminAccess
	cfg.AnonAccess = &anonAdmin

	cfg.AdminOnlyCreate = false
	if got := levelOf(t, be, ctx, "fresh", user); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.AdminAccess)
	}

	cfg.AdminOnlyCreate = true
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
}

// backendTestContext wires the config, database and store a backend call
// reads from its context (webhook delivery and rename need them).
func backendTestContext(be *Backend, cfg *config.Config) context.Context {
	ctx := context.Background()
	return ctx
}

// defaultAdmin returns the admin account the first migration creates. Tests
// pass it as the creator of fixture repositories, or put it on the context
// of a rename: an anonymous caller may not create under the default
// anon-access, and the backend refuses a create that the caller may not
// make (see checkCanCreate).
func defaultAdmin(t testing.TB, be *Backend) proto.User {
	t.Helper()
	u, err := be.User(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// asAdmin returns ctx acting as defaultAdmin.
func asAdmin(t testing.TB, be *Backend, ctx context.Context) context.Context {
	t.Helper()
	return proto.WithUserContext(ctx, defaultAdmin(t, be))
}

// seedCreate creates a fixture repository while admin-only creation is off,
// so tests can set up owners without the create policy in the way. Tests that
// exercise the policy call be.CreateRepository directly.
func seedCreate(be *Backend, ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	prev := be.cfg.AdminOnlyCreate
	be.cfg.AdminOnlyCreate = false
	defer func() { be.cfg.AdminOnlyCreate = prev }()
	return be.CreateRepository(ctx, name, user, opts)
}

// offlineRemote is the remote the import checks below use. The tests only
// care whether the create policy lets the import through; a loopback literal
// is then refused by the remote check without a DNS lookup or a dial, so
// they run without network access.
const offlineRemote = "https://127.0.0.1/x.git"

// The backend's own creation guard is the whole rule, not only its
// admin_only_create half: with that setting off, an anonymous caller may not
// create, import or rename onto a new name unless anon-access is read-write
// or above, as the SSH and HTTP handlers already require. An entry point that
// skips the handlers' checks cannot let anonymous users create.
func TestBackendRefusesAnonymousCreatesUnderReadOnlyAnonAccess(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	if cfg.AdminOnlyCreate {
		t.Fatal("not true: !cfg.AdminOnlyCreate")
	}
	if got := be.AnonAccess(ctx); got != access.ReadOnlyAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.ReadOnlyAccess)
	}

	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "anon", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(ctx, "anon", nil, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	if !errors.Is(be.RenameRepository(ctx, "a", "anon"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(ctx, \"a\", \"anon\"), proto.ErrUnauthorized)")
	}
	_, err = be.Repository(ctx, "anon")
	if !errors.Is(err, proto.ErrRepoNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoNotFound)")
	}
	_, err = be.Repository(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}

	// With anon-access read-write, anonymous callers may create.
	if err := be.SetAnonAccess(ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "anon", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// With AdminOnlyCreate on, the backend itself refuses a non-admin, and an
// anonymous caller, creating, importing or renaming to a new name, whichever
// caller reaches it. Nothing is created.
func TestBackendRefusesNonAdminCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "junk", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.CreateRepository(ctx, "anon-junk", nil, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}
	_, err = be.ImportRepository(userCtx, "junk-import", user, offlineRemote, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
	}

	_, err = be.Repository(ctx, "junk")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "anon-junk")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	_, err = be.Repository(ctx, "junk-import")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// With AdminOnlyCreate on, a non-admin's rename of an owned repository to a
// new name is refused and the repository stays where it was.
func TestBackendRefusesNonAdminRenameToNewNameWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = seedCreate(be, ctx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	userCtx := proto.WithUserContext(ctx, user)
	if !errors.Is(be.RenameRepository(userCtx, "mine", "renamed"), proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(be.RenameRepository(userCtx, \"mine\", \"renamed\"), proto.ErrUnauthorized)")
	}
	_, err = be.Repository(ctx, "mine")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "renamed")
	if err == nil {
		t.Fatal("not true: err != nil")
	}
}

// With AdminOnlyCreate on, a database admin may create, import and rename to
// a new name. The import may still fail for an unrelated reason (for example,
// a git version gate), so it only has to pass the policy.
func TestBackendAllowsAdminCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, "made", admin, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.ImportRepository(adminCtx, "imported", admin, offlineRemote, proto.RepositoryOptions{})
	if errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
	}
	if err := be.RenameRepository(adminCtx, "made", "made-again"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "made-again")
	if err != nil {
		t.Fatal(err)
	}
}

// With AdminOnlyCreate off, the same non-admin calls are not refused by the
// policy: create and rename to a new name succeed.
func TestBackendAllowsNonAdminCreatesWhenFlagOff(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = false

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.RenameRepository(userCtx, "mine", "renamed"); err != nil {
		t.Fatal(err)
	}
	_, err = be.Repository(ctx, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.ImportRepository(userCtx, "imported", user, offlineRemote, proto.RepositoryOptions{})
	if errors.Is(err, proto.ErrUnauthorized) {
		t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
	}
}

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// A key listed in initial_admin_keys is a server admin even when it was added
// after the first migration, so it never reached the database: the caller is
// anonymous or a non-admin account. With AdminOnlyCreate on, the backend must
// still let it create, import and rename to a new name: IsServerAdmin honours
// the key.
func TestBackendAllowsAdminKeyCreatesWhenFlagOn(t *testing.T) {
	t.Parallel()
	for _, registered := range []bool{false, true} {
		name := "anonymous"
		if registered {
			name = "non-admin user"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			be, cfg := newTestBackend(t)
			ctx := backendTestContext(be, cfg)
			cfg.AdminOnlyCreate = true

			pub := newTestPublicKey(t)
			setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(pub)))

			var user proto.User
			if registered {
				var err error
				user, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
				if err != nil {
					t.Fatal(err)
				}
				if user.IsAdmin() {
					t.Fatal("not true: !user.IsAdmin()")
				}
				ctx = proto.WithUserContext(ctx, user)
			}
			keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)

			// Without the key on the context, the same user is refused.
			_, err := be.CreateRepository(ctx, "nokey", user, proto.RepositoryOptions{})
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
			}

			// ReadableForUser honours the key; Readable, for the account
			// alone, does not, on a missing name as on an existing one.
			_, level := noLookupError(t)(be.ReadableForUser(keyCtx, "fresh", user))
			if got := level; got != access.AdminAccess {
				t.Fatalf("level = %v, want %v", got, access.AdminAccess)
			}
			if levelOf(t, be, keyCtx, "fresh", user) >= access.ReadWriteAccess {
				t.Fatal("not true: levelOf(t, be, keyCtx, \"fresh\", user) < access.ReadWriteAccess")
			}

			_, err = be.CreateRepository(keyCtx, "made", user, proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = be.ImportRepository(keyCtx, "imported", user, offlineRemote, proto.RepositoryOptions{})
			if errors.Is(err, proto.ErrUnauthorized) {
				t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
			}
			if !registered {
				// The rename webhook needs a sender, so rename is only
				// exercised with a registered user.
				return
			}
			if err := be.RenameRepository(keyCtx, "made", "made-again"); err != nil {
				t.Fatal(err)
			}
			_, err = be.Repository(ctx, "made-again")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Over HTTP (token, password or JWT) there is no public key on the context,
// so the only server-admin signal is the account's admin flag. Owning a key
// listed in initial_admin_keys must not count: `pubkey add` attaches a key
// without proof of holding its private half, so a user could claim a key
// before an operator lists it and become an admin over HTTP.
func TestHTTPAdminOnlyByAdminFlag(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{newTestPublicKey(t)}})
	if err != nil {
		t.Fatal(err)
	}

	// The operator lists the key after mallory attached it.
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(claimed)))

	// Load the users the way the HTTP layer does: by name (password, JWT)
	// and by access token.
	load := func(name string) []proto.User {
		u, err := be.User(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := be.CreateAccessToken(ctx, u, "t", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		byToken, err := be.UserByAccessToken(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		return []proto.User{u, byToken}
	}

	for i, u := range load("mallory") {
		if u.IsAdmin() {
			t.Fatal("not true: !u.IsAdmin()")
		}
		if got := levelOf(t, be, ctx, "fresh", u); got != access.ReadOnlyAccess {
			t.Fatalf("levelOf(t, be, ctx, \"fresh\", u) = %v, want %v", got, access.ReadOnlyAccess)
		}
		_, err := be.CreateRepository(ctx, fmt.Sprintf("mallory-%d", i), u, proto.RepositoryOptions{})
		if !errors.Is(err, proto.ErrUnauthorized) {
			t.Fatal("not true: errors.Is(err, proto.ErrUnauthorized)")
		}
	}
	for i, u := range load("boss") {
		if got := levelOf(t, be, ctx, "fresh", u); got != access.AdminAccess {
			t.Fatalf("levelOf(t, be, ctx, \"fresh\", u) = %v, want %v", got, access.AdminAccess)
		}
		_, err := be.CreateRepository(ctx, fmt.Sprintf("boss-%d", i), u, proto.RepositoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A key listed in initial_admin_keys that a non-admin account owns does not
// resolve to that account: the owner may have claimed it with `pubkey add`
// before it was listed. The lookup returns AdminKeyNonAdminOwnerError naming
// the account (the SSH server refuses the session with it), and the warning
// is logged on every such lookup. A listed key owned by an admin account
// resolves to it, and so does the owner once promoted or once the key is
// unlisted.
func TestListedAdminKeyOwnedByNonAdminHasNoAccount(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	if err != nil {
		t.Fatal(err)
	}
	bossKey := newTestPublicKey(t)
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{bossKey}})
	if err != nil {
		t.Fatal(err)
	}

	u, err := be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}

	orig := cfg.InitialAdminKeys
	setAdminKeys(t, cfg, append(append([]string{}, orig...),
		proto.MarshalAuthorizedKey(claimed), proto.MarshalAuthorizedKey(bossKey)))

	for range 3 {
		_, err = be.UserByPublicKey(ctx, claimed)
		if !errors.Is(err, proto.ErrUserNotFound) {
			t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
		}
		var owned *AdminKeyNonAdminOwnerError
		if !errors.As(err, &owned) {
			t.Fatal("not true: errors.As(err, &owned)")
		}
		if got := owned.Username; got != "mallory" {
			t.Fatalf("owned.Username = %v, want %v", got, "mallory")
		}
		if !strings.Contains(err.Error(), "user set-admin mallory true") {
			t.Fatal("not true: strings.Contains(err.Error(), \"user set-admin mallory true\")")
		}
		if !strings.Contains(err.Error(), "user remove-pubkey mallory KEY") {
			t.Fatal("not true: strings.Contains(err.Error(), \"user remove-pubkey mallory KEY\")")
		}
	}
	if got := strings.Count(logs.String(), "admin key is owned by a non-admin account"); got != 3 {
		t.Fatalf("strings.Count(logs.String(), \"admin key is owned by a non-admin account\") = %v, want %v", got, 3)
	}
	if got := baseLevel(t, be, context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), nil); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), nil) = %v, want %v", got, access.AdminAccess)
	}

	// A private repository made with the key goes to the default admin,
	// and mallory gets nothing on it.
	r, err := be.CreateRepository(context.WithValue(ctx, charmssh.ContextKeyPublicKey, claimed), "secret", nil, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	mallory, err := be.User(ctx, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID() == mallory.ID() {
		t.Fatal("not true: r.UserID() != mallory.ID()")
	}
	if got := levelOf(t, be, ctx, "secret", mallory); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", mallory) = %v, want %v", got, access.NoAccess)
	}

	u, err = be.UserByPublicKey(ctx, bossKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "boss" {
		t.Fatalf("u.Username() = %v, want %v", got, "boss")
	}

	setAdminKeys(t, cfg, orig)
	u, err = be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}

	// Promoting the owner (the fix when the key is theirs) resolves the
	// listed key to them again, keeping their account.
	setAdminKeys(t, cfg, append(append([]string{}, orig...), proto.MarshalAuthorizedKey(claimed)))
	if err := be.SetAdmin(ctx, "mallory", true); err != nil {
		t.Fatal(err)
	}
	u, err = be.UserByPublicKey(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}
	if !u.IsAdmin() {
		t.Fatal("not true: u.IsAdmin()")
	}
}

// The owned-admin-key warning is logged on every refused lookup, naming the
// current owner, and not once the conflict is resolved: the owner promoted,
// the key gone from the account, or the key no longer listed.
func TestOwnedAdminKeyWarningFollowsTheConflict(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	ctx := backendTestContext(be, cfg)
	warnings := func(owner string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "admin key is owned by a non-admin account") && strings.Contains(line, owner) {
				n++
			}
		}
		return n
	}

	key := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "eve", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	orig := cfg.InitialAdminKeys
	listed := append(append([]string{}, orig...), proto.MarshalAuthorizedKey(key))
	setAdminKeys(t, cfg, listed)

	lookup := func(owner string) {
		t.Helper()
		for range 2 {
			_, err := be.UserByPublicKey(ctx, key)
			var owned *AdminKeyNonAdminOwnerError
			if !errors.As(err, &owned) {
				t.Fatal("not true: errors.As(err, &owned)")
			}
			if got := owned.Username; got != owner {
				t.Fatalf("owned.Username = %v, want %v", got, owner)
			}
		}
	}

	lookup("mallory")
	if got := warnings("mallory"); got != 2 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 2)
	}

	// Promoting the owner resolves it; demoting them again brings it back.
	if err := be.SetAdmin(ctx, "mallory", true); err != nil {
		t.Fatal(err)
	}
	u, err := be.UserByPublicKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "mallory" {
		t.Fatalf("u.Username() = %v, want %v", got, "mallory")
	}
	if got := warnings("mallory"); got != 2 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 2)
	}
	if err := be.SetAdmin(ctx, "mallory", false); err != nil {
		t.Fatal(err)
	}
	lookup("mallory")
	if got := warnings("mallory"); got != 4 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 4)
	}

	// The key leaving the account resolves it; another non-admin claiming
	// it is logged, naming the new owner.
	if err := be.RemovePublicKey(ctx, "mallory", key); err != nil {
		t.Fatal(err)
	}
	_, err = be.UserByPublicKey(ctx, key)
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
	if err := be.AddPublicKey(ctx, "eve", key); err != nil {
		t.Fatal(err)
	}
	lookup("eve")
	if got := warnings("eve"); got != 2 {
		t.Fatalf("warnings(\"eve\") = %v, want %v", got, 2)
	}

	// Unlisting the key resolves it too.
	setAdminKeys(t, cfg, orig)
	u, err = be.UserByPublicKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Username(); got != "eve" {
		t.Fatalf("u.Username() = %v, want %v", got, "eve")
	}
	if got := warnings("eve"); got != 2 {
		t.Fatalf("warnings(\"eve\") = %v, want %v", got, 2)
	}
	if got := warnings("mallory"); got != 4 {
		t.Fatalf("warnings(\"mallory\") = %v, want %v", got, 4)
	}
}

// BaseAccessLevelForUser takes the user an SSH session already resolved and
// gives the level for a name that does not exist: admin for an admin or an
// admin key on the context, even one no account owns; read-write for a
// registered user, read-only under admin_only_create; the anonymous level
// otherwise.
func TestBaseAccessLevelForUser(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	userKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(listedKey)))

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{userKey}})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	if err != nil {
		t.Fatal(err)
	}

	for _, anon := range []access.Level{access.NoAccess, access.ReadOnlyAccess} {
		if err := be.SetAnonAccess(ctx, anon); err != nil {
			t.Fatal(err)
		}
		for _, adminOnly := range []bool{false, true} {
			cfg.AdminOnlyCreate = adminOnly
			registered := access.ReadWriteAccess
			if adminOnly {
				registered = access.ReadOnlyAccess
			}
			for _, tc := range []struct {
				name string
				pk   ssh.PublicKey
				user proto.User
				want access.Level
			}{
				{"anonymous", nil, nil, anon},
				{"unknown key", newTestPublicKey(t), nil, anon},
				{"user", userKey, user, registered},
				{"admin", adminKey, admin, access.AdminAccess},
				{"unowned admin key", listedKey, nil, access.AdminAccess},
			} {
				keyCtx := ctx
				if tc.pk != nil {
					keyCtx = context.WithValue(ctx, charmssh.ContextKeyPublicKey, tc.pk)
				}
				want := tc.want
				got := baseLevel(t, be, keyCtx, tc.user)
				if got != want {
					t.Errorf("anon=%s adminOnly=%t %s: got %s, want %s", anon, adminOnly, tc.name, got, want)
				}
			}
		}
	}

	// The unowned admin key is recognised from the context alone.
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, listedKey)
	if got := baseLevel(t, be, keyCtx, nil); got != access.AdminAccess {
		t.Fatalf("baseLevel(t, be, keyCtx, nil) = %v, want %v", got, access.AdminAccess)
	}
}

// newLookupBackend is newTestBackendWithLog logging at Debug level. The
// lookup tests break its tables.
func newLookupBackend(t *testing.T, logs io.Writer) *Backend {
	t.Helper()
	var logger *charmlog.Logger
	if logs != nil {
		logger = charmlog.New(logs)
		logger.SetLevel(charmlog.DebugLevel)
	}
	be, _ := openTestBackend(t, logger, "")
	return be
}

// During an outage a missing name gets the same answer as an existing
// private one: an error, for anonymous and registered callers alike. An
// answer of "no access" for the missing name only would tell anyone which
// private names exist.
func TestOutageAnswersMissingAndExistingAlike(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"repos", "settings"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			be := newLookupBackend(t, nil)
			ctx := context.Background()
			_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = be.CreateRepository(ctx, "public", defaultAdmin(t, be), proto.RepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}
			collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
				return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "private", access.ReadOnlyAccess)
			}); err != nil {
				t.Fatal(err)
			}

			breakTable(t, be, table)
			for _, user := range []proto.User{nil, stranger, collab} {
				for _, name := range []string{"private", "public", "ghost"} {
					r, level, err := be.Readable(ctx, name, user)
					if err == nil {
						t.Fatal("not true: err != nil")
					}
					if r != nil {
						t.Fatal("not true: r == nil")
					}
					if got := level; got != access.NoAccess {
						t.Fatalf("level = %v, want %v", got, access.NoAccess)
					}
					_, _, err = be.ReadableForUser(ctx, name, user)
					if err == nil {
						t.Fatal("not true: err != nil")
					}
				}
			}
		})
	}
}

// When the creation rule cannot be worked out because the anon-access
// setting cannot be read, create, import and rename onto a new name fail
// with the lookup error, marked as proto.ErrLookupFailed, not with a
// refusal, and nothing is created.
func TestCheckCanCreateReturnsLookupError(t *testing.T) {
	t.Parallel()
	be := newLookupBackend(t, nil)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "a", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	strangerCtx := proto.WithUserContext(ctx, stranger)

	breakTable(t, be, "settings")
	_, cerr := be.CreateRepository(strangerCtx, "new", stranger, proto.RepositoryOptions{})
	_, ierr := be.ImportRepository(strangerCtx, "new", stranger, offlineRemote, proto.RepositoryOptions{})
	rerr := be.RenameRepository(strangerCtx, "a", "new")
	for _, err := range []error{cerr, ierr, rerr} {
		if err == nil {
			t.Fatal("not true: err != nil")
		}
		if errors.Is(err, proto.ErrUnauthorized) {
			t.Fatal("not true: !errors.Is(err, proto.ErrUnauthorized)")
		}
		if !errors.Is(err, proto.ErrLookupFailed) {
			t.Fatal("not true: errors.Is(err, proto.ErrLookupFailed)")
		}
		if !strings.Contains(err.Error(), "anon-access") {
			t.Fatal("not true: strings.Contains(err.Error(), \"anon-access\")")
		} // the cause is kept
	}
	if be.repoPathExists("new") {
		t.Fatal("not true: !be.repoPathExists(\"new\")")
	}
}

// TestIsCollaboratorNotCollaborator verifies that a user who is not a
// collaborator is reported as such without an error, so callers do not have
// to tell a missing row apart from a failed lookup.
func TestIsCollaboratorNotCollaborator(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "repo", defaultAdmin(t, be), proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "collab", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	level, isCollab, err := collabOf(t, be, ctx, "repo", "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if isCollab {
		t.Fatal("not true: !isCollab")
	}
	if got, want := level, access.Level(-1); got != want {
		t.Fatalf("level = %v, want %v", got, want)
	}

	// A repository that does not exist has no collaborators either.
	level, isCollab, err = collabOf(t, be, ctx, "missing", "collab")
	if err != nil {
		t.Fatal(err)
	}
	if isCollab {
		t.Fatal("not true: !isCollab")
	}
	if got, want := level, access.Level(-1); got != want {
		t.Fatalf("level = %v, want %v", got, want)
	}

	level, isCollab, err = collabOf(t, be, ctx, "repo", "collab")
	if err != nil {
		t.Fatal(err)
	}
	if !isCollab {
		t.Fatal("not true: isCollab")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}
}

// TestUserByAccessToken verifies the token lookup finds the token's owner,
// refuses an expired token and reports an unknown one as a missing user.
func TestUserByAccessToken(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateUser(ctx, "other", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := be.CreateAccessToken(ctx, owner, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	u, err := be.UserByAccessToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.ID(), owner.ID(); got != want {
		t.Fatalf("u.ID() = %v, want %v", got, want)
	}
	if got := u.Username(); got != "owner" {
		t.Fatalf("u.Username() = %v, want %v", got, "owner")
	}

	expired, err := be.CreateAccessToken(ctx, owner, "old", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.UserByAccessToken(ctx, expired)
	if !errors.Is(err, proto.ErrTokenExpired) {
		t.Fatal("not true: errors.Is(err, proto.ErrTokenExpired)")
	}

	_, err = be.UserByAccessToken(ctx, "not-a-token")
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	}
}

// TestSettingsLookupFailureIsLogged verifies that when the settings cannot
// be read, AnonAccess and AllowKeyless still fail closed and log why.
func TestSettingsLookupFailureIsLogged(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	be, _ := newTestBackendWithLog(t, &logs)
	ctx := context.Background()

	if err := be.db.Close(); err != nil {
		t.Fatal(err)
	}

	if got := be.AnonAccess(ctx); got != access.NoAccess {
		t.Fatalf("be.AnonAccess(ctx) = %v, want %v", got, access.NoAccess)
	}
	if got := be.AllowKeyless(ctx); got != false {
		t.Fatalf("be.AllowKeyless(ctx) = %v, want %v", got, false)
	}

	out := logs.String()
	if !strings.Contains(out, "error getting anon-access setting") {
		t.Fatal("not true: strings.Contains(out, \"error getting anon-access setting\")")
	}
	if !strings.Contains(out, "error getting allow-keyless setting") {
		t.Fatal("not true: strings.Contains(out, \"error getting allow-keyless setting\")")
	}
}

// An SSH session resolves its user once. If that account is renamed and
// another account takes the old name, the session keeps its own account's
// collaborator rows and gets nothing from the other account's: Readable
// looks collaborators up by user ID, as ReadableRepositories does.
func TestCollaboratorLookedUpByUserID(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	session, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.SetUsername(ctx, "alice", "carol"); err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "alice", "private", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	r, level := noLookupError(t)(be.Readable(ctx, "private", session))
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	repos, err := be.ReadableRepositories(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 0 {
		t.Fatalf("len(repos) = %v, want %v", got, 0)
	}

	// The account that holds the name now has the row.
	alice, err := be.User(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	r, level = noLookupError(t)(be.Readable(ctx, "private", alice))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}
}

// breakTable renames table so every query on it fails, as a database fault
// would.
func breakTable(t *testing.T, be *Backend, table string) {
	t.Helper()
	if _, err := be.db.ExecContext(context.Background(), "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
		t.Fatal(err)
	}
}

// A lookup that fails is returned, not answered as a missing or unreadable
// repository: Readable and ReadableForUser give the error with no
// repository and no access, and leave logging it to the caller.
func TestReadableReturnsLookupErrors(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()

	admin := defaultAdmin(t, be)
	_, err := be.CreateRepository(ctx, "private", admin, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Without the collaborators table, the stranger's level is unknown.
	breakTable(t, be, "collabs")
	for _, readable := range []func(context.Context, string, proto.User) (proto.Repository, access.Level, error){be.Readable, be.ReadableForUser} {
		r, level, err := readable(ctx, "private", stranger)
		if err == nil {
			t.Fatal("not true: err != nil")
		}
		if r != nil {
			t.Fatal("not true: r == nil")
		}
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
	}
	// The repository and the caller's collaborator row are read in one
	// query, so the lookup fails for its owner and admins too.
	r, level, err := be.Readable(ctx, "private", admin)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}

	// So it does without the repositories table.
	breakTable(t, be, "repos")
	r, level, err = be.ReadableForUser(ctx, "private", admin)
	if err == nil {
		t.Fatal("not true: err != nil")
	}
	if r != nil {
		t.Fatal("not true: r == nil")
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	_, err = be.ReadableRepositoriesForUser(ctx, stranger)
	if err == nil {
		t.Fatal("not true: err != nil")
	}

	if strings.Contains(logs.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"ERRO\")")
	}
}

// The user and token lookups leave logging a failure to their callers,
// which know whether the client went away.
func TestUserLookupErrorsNotLogged(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	be := newLookupBackend(t, &logs)
	ctx := context.Background()
	u, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, u, "t", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	breakTable(t, be, "users")
	_, err = be.User(ctx, "alice")
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByID(ctx, u.ID())
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByPublicKey(ctx, newTestPublicKey(t))
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}
	_, err = be.UserByAccessToken(ctx, token)
	if err == nil || errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: err != nil && !errors.Is(err, proto.ErrUserNotFound)")
	}

	if strings.Contains(logs.String(), "ERRO") {
		t.Fatal("not true: !strings.Contains(logs.String(), \"ERRO\")")
	}
}

// RemoveOwnPublicKey refuses the account's only key, removes any other, and
// leaves the admin path (RemovePublicKey) free to remove the last key.
func TestRemoveOwnPublicKey(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	k1, k2 := newTestPublicKey(t), newTestPublicKey(t)
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{k1, k2}})
	if err != nil {
		t.Fatal(err)
	}

	if err := be.RemoveOwnPublicKey(ctx, alice, k2); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RemoveOwnPublicKey(ctx, alice, k1), ErrLastPublicKey) {
		t.Fatal("not true: errors.Is(be.RemoveOwnPublicKey(ctx, alice, k1), ErrLastPublicKey)")
	}
	keys, err := publicKeysOf(be, ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(keys); got != 1 {
		t.Fatalf("len(keys) = %v, want %v", got, 1)
	}

	// A key the account does not hold is not "the last key": it is reported
	// as not found rather than refused.
	if !errors.Is(be.RemoveOwnPublicKey(ctx, alice, k2), proto.ErrPublicKeyNotFound) {
		t.Fatal("not true: errors.Is(be.RemoveOwnPublicKey(ctx, alice, k2), proto.ErrPublicKeyNotFound)")
	}
	gone, err := be.CreateUser(ctx, "gone", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteUser(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(be.RemoveOwnPublicKey(ctx, gone, k1), proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(be.RemoveOwnPublicKey(ctx, gone, k1), proto.ErrUserNotFound)")
	}

	if err := be.RemovePublicKey(ctx, "alice", k1); err != nil {
		t.Fatal(err)
	}
	keys, err = publicKeysOf(be, ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(keys); got != 0 {
		t.Fatalf("len(keys) = %v, want %v", got, 0)
	}
}

// Two sessions of one user, on keys k1 and k2, each remove the other's key
// at the same moment. Both pass the SSH command's session-key check, so it
// is the backend's locked count that must refuse one of them: the account
// always keeps a key.
func TestRemoveOwnPublicKeyConcurrent(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	testRemoveOwnPublicKeyConcurrent(t, be, backendTestContext(be, cfg))
}

// TestRemoveOwnPublicKeyConcurrentPostgres is TestRemoveOwnPublicKeyConcurrent
// on Postgres, whose READ COMMITTED transactions would both count two keys
// without the row lock.
func TestRemoveOwnPublicKeyConcurrentPostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	testRemoveOwnPublicKeyConcurrent(t, e.be, e.ctx)
}

func testRemoveOwnPublicKeyConcurrent(t *testing.T, be *Backend, ctx context.Context) {
	t.Helper()
	for i := range 20 {
		name := fmt.Sprintf("racer%d", i)
		k1, k2 := newTestPublicKey(t), newTestPublicKey(t)
		u, err := be.CreateUser(ctx, name, proto.UserOptions{PublicKeys: []ssh.PublicKey{k1, k2}})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for j, k := range []ssh.PublicKey{k2, k1} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[j] = be.RemoveOwnPublicKey(ctx, u, k)
			}()
		}
		close(start)
		wg.Wait()

		refused := 0
		for _, err := range errs {
			switch {
			case err == nil:
			case errors.Is(err, ErrLastPublicKey):
				refused++
			default:
				t.Fatalf("round %d: unexpected error: %v", i, err)
			}
		}
		if got := refused; got != 1 {
			t.Fatalf("refused = %v, want %v", got, 1)
		}
		keys, err := publicKeysOf(be, ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(keys); got != 1 {
			t.Fatalf("len(keys) = %v, want %v", got, 1)
		}
	}
}

// TestLookupsDoNotWaitForWriter checks that the lookups behind every access
// check read without taking the SQLite write lock: they must succeed while
// another connection holds a write transaction open, instead of waiting for
// it (busy_timeout) and then failing with "database is locked".
func TestLookupsDoNotWaitForWriter(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	// Both differ from what the lookups fall back to on an error.
	if err := be.SetAnonAccess(ctx, access.ReadWriteAccess); err != nil {
		t.Fatal(err)
	}
	if err := be.SetAllowKeyless(ctx, true); err != nil {
		t.Fatal(err)
	}
	admin, err := be.User(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{Description: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	pk := newTestPublicKey(t)
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{PublicKeys: []ssh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, collab, "t", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Inactive, so RedeliverWebhookDelivery stops with ErrWebhookInactive
	// after its lookups instead of sending anything.
	var hookID int64
	delID := uuid.New()
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		if err := be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "repo", access.ReadOnlyAccess); err != nil {
			return err
		}
		var err error
		hookID, err = be.store.CreateWebhook(ctx, tx, models.Webhook{RepoID: repo.ID(), URL: "http://127.0.0.1:1/", ContentType: int(webhook.ContentTypeJSON)})
		if err != nil {
			return err
		}
		return be.store.CreateWebhookDelivery(ctx, tx, models.WebhookDelivery{
			ID:             delID,
			WebhookID:      hookID,
			Event:          int(webhook.EventPush),
			RequestURL:     "http://127.0.0.1:1/",
			RequestMethod:  "POST",
			RequestBody:    "{}",
			ResponseStatus: 200,
		})
	}); err != nil {
		t.Fatal(err)
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- be.db.TransactionContext(ctx, func(tx *db.Tx) error {
			if err := be.store.CreateUser(ctx, tx, "writer", false, nil); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	func() {
		defer close(release)
		// The writer holds the write lock until every lookup is done, so a
		// lookup that waits for it never gets it: one that waits for the
		// SQLite lock fails with "database is locked" after busy_timeout,
		// and one that waits for this process's write turn fails when its
		// context ends. Each lookup gets its own deadline, so one that waits
		// does not use up the time of the lookups after it. The deadline is
		// far longer than a lookup takes on a slow machine; it only bounds
		// how long a broken lookup holds the test up.
		var cancels []context.CancelFunc
		defer func() {
			for _, cancel := range cancels {
				cancel()
			}
		}()
		rctx := func() context.Context {
			c, cancel := context.WithTimeout(ctx, time.Minute)
			cancels = append(cancels, cancel)
			return c
		}

		u, err := be.User(rctx(), "admin")
		if err != nil {
			t.Errorf("User: %v", err)
		} else if _, err := be.UserByID(rctx(), u.ID()); err != nil {
			t.Errorf("UserByID: %v", err)
		}
		if _, err := be.Users(rctx()); err != nil {
			t.Errorf("Users: %v", err)
		}
		if u, err := be.UserByPublicKey(rctx(), pk); err != nil || u.Username() != "collab" {
			t.Errorf("UserByPublicKey = %v, %v; want collab", u, err)
		}
		if u, err := be.UserByAccessToken(rctx(), token); err != nil || u.Username() != "collab" {
			t.Errorf("UserByAccessToken = %v, %v; want collab", u, err)
		}
		if _, err := publicKeysOf(be, rctx(), "admin"); err != nil {
			t.Errorf("public keys of admin: %v", err)
		}
		if got := be.AnonAccess(rctx()); got != access.ReadWriteAccess {
			t.Errorf("AnonAccess = %v, want %v", got, access.ReadWriteAccess)
		}
		if !be.AllowKeyless(rctx()) {
			t.Errorf("AllowKeyless = false, want true")
		}
		if level, ok, err := collabOf(t, be, rctx(), "repo", "collab"); err != nil || !ok || level != access.ReadOnlyAccess {
			t.Errorf("IsCollaborator = %v, %v, %v; want %v, true, nil", level, ok, err, access.ReadOnlyAccess)
		}
		if _, err := be.Collaborators(rctx(), "repo"); err != nil {
			t.Errorf("Collaborators: %v", err)
		}
		if _, err := be.Repository(rctx(), "repo"); err != nil {
			t.Errorf("Repository: %v", err)
		}
		if _, err := be.Repositories(rctx()); err != nil {
			t.Errorf("Repositories: %v", err)
		}
		if _, err := be.Webhook(rctx(), repo, hookID); err != nil {
			t.Errorf("Webhook: %v", err)
		}
		if _, err := be.ListWebhooks(rctx(), repo); err != nil {
			t.Errorf("ListWebhooks: %v", err)
		}
		if ds, err := be.ListWebhookDeliveries(rctx(), repo, hookID); err != nil || len(ds) != 1 {
			t.Errorf("ListWebhookDeliveries = %d deliveries, %v; want 1, nil", len(ds), err)
		}
		if _, err := be.WebhookDelivery(rctx(), repo, hookID, delID); err != nil {
			t.Errorf("WebhookDelivery: %v", err)
		}
		if err := be.RedeliverWebhookDelivery(rctx(), repo, hookID, delID); !errors.Is(err, webhook.ErrWebhookInactive) {
			t.Errorf("RedeliverWebhookDelivery = %v, want %v", err, webhook.ErrWebhookInactive)
		}
	}()

	if err := <-writerErr; err != nil {
		t.Fatal(err)
	}
}

// ReadableForUser takes the user an SSH session already resolved and gives
// each caller its level: admin for an admin or an admin key on the context,
// even one no account owns, and for the owner; the collaborator's level on
// the private repository; read-only for other registered users on the public
// one; the anonymous level for callers without an account; nothing on the
// private repository for anyone else. A missing name gives the level for a
// new name. The repository is returned when it exists and the level reads.
func TestReadableForUser(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	ownerKey := newTestPublicKey(t)
	collabKey := newTestPublicKey(t)
	otherKey := newTestPublicKey(t)
	adminKey := newTestPublicKey(t)
	listedKey := newTestPublicKey(t)
	setAdminKeys(t, cfg, append(cfg.InitialAdminKeys, proto.MarshalAuthorizedKey(listedKey)))

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{ownerKey}})
	if err != nil {
		t.Fatal(err)
	}
	collab, err := be.CreateUser(ctx, "collab", proto.UserOptions{PublicKeys: []ssh.PublicKey{collabKey}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := be.CreateUser(ctx, "other", proto.UserOptions{PublicKeys: []ssh.PublicKey{otherKey}})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{adminKey}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "public", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	// Straight to the store: AddCollaborator also sends a webhook event,
	// which needs a database on ctx.
	if err := be.db.TransactionContext(ctx, func(tx *db.Tx) error {
		return be.store.AddCollabByUsernameAndRepo(ctx, tx, "collab", "private", access.ReadWriteAccess)
	}); err != nil {
		t.Fatal(err)
	}

	for _, anon := range []access.Level{access.NoAccess, access.ReadOnlyAccess} {
		if err := be.SetAnonAccess(ctx, anon); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"public", "private", "missing"} {
			for _, tc := range []struct {
				name string
				pk   ssh.PublicKey
				user proto.User
			}{
				{"anonymous", nil, nil},
				{"unknown key", newTestPublicKey(t), nil},
				{"owner", ownerKey, owner},
				{"collaborator", collabKey, collab},
				{"other user", otherKey, other},
				{"admin", adminKey, admin},
				{"unowned admin key", listedKey, nil},
			} {
				keyCtx := ctx
				if tc.pk != nil {
					keyCtx = context.WithValue(ctx, charmssh.ContextKeyPublicKey, tc.pk)
				}
				wantLevel := expectReadable(name, tc.name, anon)
				wantRepo := name != "missing" && wantLevel >= access.ReadOnlyAccess
				gotRepo, gotLevel := noLookupError(t)(be.ReadableForUser(keyCtx, name, tc.user))
				if gotLevel != wantLevel || (gotRepo != nil) != wantRepo {
					t.Errorf("anon=%s repo=%s %s: got (%v, %s), want (%v, %s)",
						anon, name, tc.name, gotRepo != nil, gotLevel, wantRepo, wantLevel)
				}
			}
		}
	}

	// The unowned admin key opens a private repository from the context alone.
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, listedKey)
	r, level := noLookupError(t)(be.ReadableForUser(keyCtx, "private", nil))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}
}

// expectReadable is the level TestReadableForUser expects caller to hold on
// repo, with anon at most read-only and admin_only_create off.
func expectReadable(repo, caller string, anon access.Level) access.Level {
	switch {
	case caller == "admin" || caller == "unowned admin key":
		return access.AdminAccess
	case caller == "anonymous" || caller == "unknown key":
		if repo == "private" {
			return access.NoAccess
		}
		return anon
	case repo == "missing":
		return access.ReadWriteAccess
	case caller == "owner":
		return access.AdminAccess
	case repo == "private" && caller == "collaborator":
		return access.ReadWriteAccess
	case repo == "private":
		return access.NoAccess
	}
	return access.ReadOnlyAccess
}

// ReadableForUser uses the user it is given and does not resolve the key
// again: once the owner's key no longer maps to the account, a by-key
// lookup loses the private repository, but the session user keeps it.
func TestReadableForUserDoesNotLookUpKey(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	pk := newTestPublicKey(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{PublicKeys: []ssh.PublicKey{pk}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "private", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	sessCtx := proto.WithUserContext(context.WithValue(ctx, charmssh.ContextKeyPublicKey, pk), owner)
	if err := be.RemovePublicKey(ctx, "owner", pk); err != nil {
		t.Fatal(err)
	}

	_, err = be.UserByPublicKey(ctx, pk)
	if !errors.Is(err, proto.ErrUserNotFound) {
		t.Fatal("not true: errors.Is(err, proto.ErrUserNotFound)")
	} // the key no longer resolves to the owner

	r, level := noLookupError(t)(be.ReadableForUser(sessCtx, "private", proto.UserFromContext(sessCtx)))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}
}

// lookupCountingStore counts the lookups an access check makes, each of
// which an outage can fail.
type lookupCountingStore struct {
	store.Store
	n atomic.Int32
}

func (s *lookupCountingStore) GetRepoByName(ctx context.Context, h db.Handler, name string) (models.Repo, error) {
	s.n.Add(1)
	return s.Store.GetRepoByName(ctx, h, name)
}

func (s *lookupCountingStore) GetRepoWithCollabByName(ctx context.Context, h db.Handler, name string, userID int64) (models.Repo, access.Level, bool, error) {
	s.n.Add(1)
	return s.Store.GetRepoWithCollabByName(ctx, h, name, userID)
}

func (s *lookupCountingStore) GetCollabByUserIDAndRepoID(ctx context.Context, h db.Handler, userID int64, repoID int64) (models.Collab, error) {
	s.n.Add(1)
	return s.Store.GetCollabByUserIDAndRepoID(ctx, h, userID, repoID)
}

func (s *lookupCountingStore) GetAnonAccess(ctx context.Context, h db.Handler) (access.Level, error) {
	s.n.Add(1)
	return s.Store.GetAnonAccess(ctx, h)
}

// A registered stranger asking for a private repository costs as many
// lookups as asking for a missing name, so an outage cannot tell the two
// apart by which lookup failed.
func TestReadableLookupsMatchForPrivateAndMissing(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := &lookupCountingStore{Store: be.store}
	be.store = st

	lookups := func(name string) int32 {
		st.n.Store(0)
		r, level, err := be.Readable(ctx, name, stranger)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			t.Fatal("not true: r == nil")
		}
		if level >= access.ReadOnlyAccess && name != "missing" {
			t.Fatal("not true: level < access.ReadOnlyAccess || name == \"missing\"")
		}
		return st.n.Load()
	}
	if got, want := lookups("private"), lookups("missing"); got != want {
		t.Fatalf("lookups(\"private\") = %v, want %v", got, want)
	}
}

// With only the collaborators table failing, an existing private repository
// and a missing name answer alike, for an anonymous caller and for a
// stranger: both lookups go through the same failing query, so a missing
// name does not skip it and answer as free.
func TestReadableCollabOutageAnswersAlike(t *testing.T) {
	t.Parallel()
	be := newLookupBackend(t, nil)
	ctx := context.Background()
	_, err := be.CreateRepository(ctx, "private", defaultAdmin(t, be), proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	breakTable(t, be, "collabs")

	for _, user := range []proto.User{nil, stranger} {
		answer := func(name string) string {
			r, level, err := be.Readable(ctx, name, user)
			return fmt.Sprintf("%v %v %v", r != nil, level, err != nil)
		}
		if got, want := answer("private"), answer("missing"); got != want {
			t.Fatalf("answer(\"private\") = %v, want %v", got, want)
		}
	}
}

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
	if !got["gone-public"] || !got["gone-secret"] {
		t.Fatal("not true: got[\"gone-public\"] && got[\"gone-secret\"]")
	}
}

// TestReadable verifies the single lookup behind every access gate: the
// repository comes back only when the caller may read it, so a private
// repository and a missing one look identical, while the level still says
// whether a missing name may be created.
func TestReadable(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.CreateRepository(ctx, "public", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Anonymous: both nil; the level is the anonymous default either way.
	r, level := noLookupError(t)(be.Readable(ctx, "secret", nil))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", nil))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.ReadOnlyAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadOnlyAccess)
	}

	// Registered stranger: both nil; a missing name may be created.
	r, level = noLookupError(t)(be.Readable(ctx, "secret", stranger))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", stranger))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.ReadWriteAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
	}

	// Readers get the repository.
	r, level = noLookupError(t)(be.Readable(ctx, "public", nil))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.ReadOnlyAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadOnlyAccess)
	}
	r, level = noLookupError(t)(be.Readable(ctx, "secret", owner))
	if r == nil {
		t.Fatal("not true: r != nil")
	}
	if got := level; got != access.AdminAccess {
		t.Fatalf("level = %v, want %v", got, access.AdminAccess)
	}

	// levelOf, the level alone, is the same computation.
	if got := levelOf(t, be, ctx, "secret", stranger); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", stranger) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "ghost", stranger); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"ghost\", stranger) = %v, want %v", got, access.ReadWriteAccess)
	}
	if got := levelOf(t, be, ctx, "public", nil); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"public\", nil) = %v, want %v", got, access.ReadOnlyAccess)
	}

	// With admin-only creation on, a missing name is no longer creatable.
	cfg.AdminOnlyCreate = true
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", stranger))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.ReadOnlyAccess {
		t.Fatalf("level = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got := levelOf(t, be, ctx, "secret", stranger); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", stranger) = %v, want %v", got, access.NoAccess)
	}
}

// TestBaseAccessLevelForUserAnonymous verifies the baseline level does not
// depend on any repository and matches the level for a missing name.
func TestBaseAccessLevelForUserAnonymous(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	// No key and no account: the anonymous level.
	if got := baseLevel(t, be, ctx, nil); got != access.ReadOnlyAccess {
		t.Fatalf("baseLevel(t, be, ctx, nil) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got, want := baseLevel(t, be, ctx, nil), levelOf(t, be, ctx, "ghost", nil); got != want {
		t.Fatalf("baseLevel(t, be, ctx, nil) = %v, want %v", got, want)
	}

	if err := be.SetAnonAccess(ctx, access.NoAccess); err != nil {
		t.Fatal(err)
	}
	if got := baseLevel(t, be, ctx, nil); got != access.NoAccess {
		t.Fatalf("baseLevel(t, be, ctx, nil) = %v, want %v", got, access.NoAccess)
	}
}

// TestAccessLevelRowWithoutDirectory verifies a repository row whose
// directory is gone counts as a taken name, not a free one: listings walk
// the rows and check each name, so falling back to the level for a missing
// name would show a private repository to everyone.
func TestAccessLevelRowWithoutDirectory(t *testing.T) {
	t.Parallel()
	be, _ := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
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
	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{
		Private:     true,
		Description: "confidential",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(be.repoPath("secret")); err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "secret", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", nil) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "secret", stranger); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", stranger) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "secret", owner); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", owner) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "secret", admin); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", admin) = %v, want %v", got, access.AdminAccess)
	}

	r, level := noLookupError(t)(be.Readable(ctx, "secret", stranger))
	if got := r; got != nil {
		t.Fatalf("r = %v, want nil", got)
	}
	if got := level; got != access.NoAccess {
		t.Fatalf("level = %v, want %v", got, access.NoAccess)
	}

	// A truly free name is unaffected.
	if got := levelOf(t, be, ctx, "ghost", stranger); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"ghost\", stranger) = %v, want %v", got, access.ReadWriteAccess)
	}
}

// Postgres runs transactions in READ COMMITTED, so DeleteUser looking for
// the user's repositories while a create has inserted one but not yet
// committed would miss it, and its DELETE, waiting for that insert's
// foreign key lock, would then remove the committed repository's row
// through ON DELETE CASCADE, leaving its directory behind. DeleteUser reads
// the user with FOR UPDATE, which waits for the insert to commit: here it
// must block on the creating transaction (checked with pg_blocking_pids)
// and then refuse with proto.ErrUserOwnsRepos.
func TestDeleteUserWaitsForRepoCreatePostgres(t *testing.T) {
	t.Parallel()
	e := newPostgresWebhookEnv(t)
	owner, err := e.be.CreateUser(e.ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	if err := (e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return err
		}
		if _, err := e.be.store.CreateRepo(e.ctx, tx, models.Repo{
			Name:   "owned",
			UserID: owner.ID(),
		}); err != nil {
			return err
		}
		go func() {
			result <- e.be.DeleteUser(e.ctx, "owner")
		}()
		// An error, rather than a failed test, rolls the create back and
		// lets DeleteUser finish before the database is dropped.
		return waitBlockedBy(e, holder, result, "DeleteUser")
	})); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, proto.ErrUserOwnsRepos) {
			t.Fatal("not true: errors.Is(err, proto.ErrUserOwnsRepos)")
		}
	case <-time.After(time.Minute):
		t.Fatal("DeleteUser still waiting after the create committed")
	}
	_, err = e.be.User(e.ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.be.store.GetRepoByName(e.ctx, e.be.db, "owned")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.UserID, owner.ID(); got != want {
		t.Fatalf("m.UserID = %v, want %v", got, want)
	}
}
