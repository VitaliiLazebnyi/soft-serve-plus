package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestOrphanRepoDirectoryIsNotClaimable verifies that a repository directory
// with no database row is treated as taken rather than missing. Otherwise a
// user who can create repositories could claim it, and on a case-insensitive
// filesystem "claim it" means becoming the owner of somebody else's
// repository.
func TestOrphanRepoDirectoryIsNotClaimable(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(cfg.DataPath, "repos", "orphan.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := levelOf(t, be, ctx, "orphan", user); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", user) = %v, want %v", got, access.NoAccess)
	}
	if got := levelOf(t, be, ctx, "orphan", nil); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", nil) = %v, want %v", got, access.NoAccess)
	}

	_, err = be.CreateRepository(ctx, "orphan", user, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoExist)")
	}

	// A genuinely new name is still creatable, unless admin-only creation is on.
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadWriteAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadWriteAccess)
	}
	cfg.AdminOnlyCreate = true
	if got := levelOf(t, be, ctx, "fresh", user); got != access.ReadOnlyAccess {
		t.Fatalf("levelOf(t, be, ctx, \"fresh\", user) = %v, want %v", got, access.ReadOnlyAccess)
	}
	if got := levelOf(t, be, ctx, "orphan", user); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"orphan\", user) = %v, want %v", got, access.NoAccess)
	}
}

// TestCaseVariantOfExistingRepo verifies that a name differing only by case
// from an existing private repository cannot be used to create or gain
// access to it. On a case-sensitive filesystem the two names are distinct
// repositories and the variant is simply creatable; on a case-insensitive
// one they share a directory and the variant must be refused.
func TestCaseVariantOfExistingRepo(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := be.CreateUser(ctx, "attacker", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true})
	if err != nil {
		t.Fatal(err)
	}

	_, statErr := os.Stat(filepath.Join(cfg.DataPath, "repos", "SECRET.git"))
	caseInsensitive := statErr == nil

	level := levelOf(t, be, ctx, "SECRET", attacker)
	_, createErr := be.CreateRepository(ctx, "SECRET", attacker, proto.RepositoryOptions{})

	if caseInsensitive {
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
		if !errors.Is(createErr, proto.ErrRepoExist) {
			t.Fatal("not true: errors.Is(createErr, proto.ErrRepoExist)")
		}
	} else {
		if got := level; got != access.ReadWriteAccess {
			t.Fatalf("level = %v, want %v", got, access.ReadWriteAccess)
		}
		if createErr != nil {
			t.Fatal(createErr)
		}
	}

	// Whatever the filesystem did, the original is untouched and still private.
	r, err := be.Repository(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got, want := r.UserID(), owner.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if got := levelOf(t, be, ctx, "secret", attacker); got != access.NoAccess {
		t.Fatalf("levelOf(t, be, ctx, \"secret\", attacker) = %v, want %v", got, access.NoAccess)
	}

	// With admin-only creation on, no case variant is creatable by the
	// attacker, whatever the filesystem.
	cfg.AdminOnlyCreate = true
	level = levelOf(t, be, ctx, "Secret", attacker)
	_, createErr = be.CreateRepository(ctx, "Secret", attacker, proto.RepositoryOptions{})
	if !errors.Is(createErr, proto.ErrUnauthorized) {
		t.Fatal("not true: errors.Is(createErr, proto.ErrUnauthorized)")
	}
	if caseInsensitive {
		if got := level; got != access.NoAccess {
			t.Fatalf("level = %v, want %v", got, access.NoAccess)
		}
	} else {
		if got := level; got != access.ReadOnlyAccess {
			t.Fatalf("level = %v, want %v", got, access.ReadOnlyAccess)
		}
	}
}

// TestConcurrentCreateSameName verifies that racing creates of one name yield
// exactly one repository; the directory claim is what arbitrates.
func TestConcurrentCreateSameName(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := be.CreateRepository(ctx, "raced", defaultAdmin(t, be), proto.RepositoryOptions{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var created, existed int
	for err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, proto.ErrRepoExist):
			existed++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := created; got != 1 {
		t.Fatalf("created = %v, want %v", got, 1)
	}
	if got, want := existed, workers-1; got != want {
		t.Fatalf("existed = %v, want %v", got, want)
	}

	repos, err := be.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repos); got != 1 {
		t.Fatalf("len(repos) = %v, want %v", got, 1)
	}
	_, err = os.Stat(filepath.Join(cfg.DataPath, "repos", "raced.git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
}

// TestRegisterRepositoryAdoptsClaimedPath covers the import flow: the
// directory is claimed and filled before the database row exists, and
// registering it must not refuse its own directory.
func TestRegisterRepositoryAdoptsClaimedPath(t *testing.T) {
	t.Parallel()
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "importer", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rp := filepath.Join(cfg.DataPath, "repos", "imported.git")
	if err := claimRepoPath(rp); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimRepoPath(rp), proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(claimRepoPath(rp), proto.ErrRepoExist)")
	}
	_, err = git.Init(t.Context(), rp, true)
	if err != nil {
		t.Fatal(err)
	}

	r, err := be.registerRepository(ctx, "imported", owner, proto.RepositoryOptions{Private: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.UserID(), owner.ID(); got != want {
		t.Fatalf("r.UserID() = %v, want %v", got, want)
	}
	if !r.IsPrivate() {
		t.Fatal("not true: r.IsPrivate()")
	}
	if got := levelOf(t, be, ctx, "imported", owner); got != access.AdminAccess {
		t.Fatalf("levelOf(t, be, ctx, \"imported\", owner) = %v, want %v", got, access.AdminAccess)
	}

	// The name is now taken on both axes.
	_, err = be.CreateRepository(ctx, "imported", owner, proto.RepositoryOptions{})
	if !errors.Is(err, proto.ErrRepoExist) {
		t.Fatal("not true: errors.Is(err, proto.ErrRepoExist)")
	}
	_, err = be.Repository(ctx, "imported")
	if err != nil {
		t.Fatal(err)
	}
}
