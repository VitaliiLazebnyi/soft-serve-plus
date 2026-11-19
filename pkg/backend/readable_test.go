package backend

import (
	"context"
	"os"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// TestReadable verifies the single lookup behind every access gate: the
// repository comes back only when the caller may read it, so a private
// repository and a missing one look identical, while the level still says
// whether a missing name may be created.
func TestReadable(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "public", owner, proto.RepositoryOptions{})
	is.NoErr(err)

	// Anonymous: both nil; the level is the anonymous default either way.
	r, level := noLookupError(t)(be.Readable(ctx, "secret", nil))
	is.Equal(r, nil)
	is.Equal(level, access.NoAccess)
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", nil))
	is.Equal(r, nil)
	is.Equal(level, access.ReadOnlyAccess)

	// Registered stranger: both nil; a missing name may be created.
	r, level = noLookupError(t)(be.Readable(ctx, "secret", stranger))
	is.Equal(r, nil)
	is.Equal(level, access.NoAccess)
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", stranger))
	is.Equal(r, nil)
	is.Equal(level, access.ReadWriteAccess)

	// Readers get the repository.
	r, level = noLookupError(t)(be.Readable(ctx, "public", nil))
	is.True(r != nil)
	is.Equal(level, access.ReadOnlyAccess)
	r, level = noLookupError(t)(be.Readable(ctx, "secret", owner))
	is.True(r != nil)
	is.Equal(level, access.AdminAccess)

	// levelOf, the level alone, is the same computation.
	is.Equal(levelOf(t, be, ctx, "secret", stranger), access.NoAccess)
	is.Equal(levelOf(t, be, ctx, "ghost", stranger), access.ReadWriteAccess)
	is.Equal(levelOf(t, be, ctx, "public", nil), access.ReadOnlyAccess)

	// With admin-only creation on, a missing name is no longer creatable.
	cfg.AdminOnlyCreate = true
	r, level = noLookupError(t)(be.Readable(ctx, "ghost", stranger))
	is.Equal(r, nil)
	is.Equal(level, access.ReadOnlyAccess)
	is.Equal(levelOf(t, be, ctx, "secret", stranger), access.NoAccess)
}

// TestBaseAccessLevelForUserAnonymous verifies the baseline level does not
// depend on any repository and matches the level for a missing name.
func TestBaseAccessLevelForUserAnonymous(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	// No key and no account: the anonymous level.
	is.Equal(be.BaseAccessLevelForUser(ctx, nil), access.ReadOnlyAccess)
	is.Equal(be.BaseAccessLevelForUser(ctx, nil), levelOf(t, be, ctx, "ghost", nil))

	is.NoErr(be.SetAnonAccess(ctx, access.NoAccess))
	is.Equal(be.BaseAccessLevelForUser(ctx, nil), access.NoAccess)
}

// TestAccessLevelRowWithoutDirectory verifies a repository row whose
// directory is gone counts as a taken name, not a free one: listings walk
// the rows and check each name, so falling back to the level for a missing
// name would show a private repository to everyone.
func TestAccessLevelRowWithoutDirectory(t *testing.T) {
	is := is.New(t)
	be, _ := newTestBackend(t)
	ctx := context.Background()

	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	stranger, err := be.CreateUser(ctx, "stranger", proto.UserOptions{})
	is.NoErr(err)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "secret", owner, proto.RepositoryOptions{
		Private:     true,
		Description: "confidential",
	})
	is.NoErr(err)
	is.NoErr(os.RemoveAll(be.repoPath("secret")))

	is.Equal(levelOf(t, be, ctx, "secret", nil), access.NoAccess)
	is.Equal(levelOf(t, be, ctx, "secret", stranger), access.NoAccess)
	is.Equal(levelOf(t, be, ctx, "secret", owner), access.NoAccess)
	is.Equal(levelOf(t, be, ctx, "secret", admin), access.AdminAccess)

	r, level := noLookupError(t)(be.Readable(ctx, "secret", stranger))
	is.Equal(r, nil)
	is.Equal(level, access.NoAccess)

	// A truly free name is unaffected.
	is.Equal(levelOf(t, be, ctx, "ghost", stranger), access.ReadWriteAccess)
}
