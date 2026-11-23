package backend

import (
	"context"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

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
